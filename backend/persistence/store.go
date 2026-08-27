// Package persistence provides bounded, metadata-first storage for immutable
// wire body artifacts.  It deliberately stores application bytes as opaque
// bytes: callers retrieve a fresh artifact or reader, while snapshots/events
// can carry only the returned ArtifactRef.
package persistence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"context-lens/backend/wire"
)

const (
	// DefaultMaxArtifactBytes is intentionally the same safety bound used by
	// the workspace API.  Config values of zero mean "not configured" rather
	// than silently imposing this default; callers that need a bounded store
	// should set at least MaxArtifactBytes (and normally MaxTotalBytes too).
	DefaultMaxArtifactBytes int64 = 8 << 20

	// defaultJanitorInterval avoids a surprise background goroutine.  A
	// janitor is started only when Config.CleanupInterval is positive.
	defaultFileMode os.FileMode = 0o600
	defaultRootMode os.FileMode = 0o700
)

// Stable errors are suitable for errors.Is checks by workspace and exchange
// adapters.  Error text never contains body bytes.
var (
	ErrClosed            = errors.New("persistence: artifact store is closed")
	ErrNotFound          = errors.New("persistence: artifact not found")
	ErrArtifactExists    = errors.New("persistence: artifact already exists")
	ErrInvalidArtifactID = errors.New("persistence: invalid artifact id")
	ErrInvalidArtifact   = errors.New("persistence: invalid artifact")
	ErrArtifactTooLarge  = errors.New("persistence: artifact exceeds size limit")
	ErrStoreFull         = errors.New("persistence: artifact store limit exceeded")
	ErrMemoryLimit       = errors.New("persistence: artifact memory limit exceeded")
	ErrCorruptArtifact   = errors.New("persistence: stored artifact is corrupt")
	ErrInvalidRange      = errors.New("persistence: invalid artifact range")
	ErrInvalidConfig     = errors.New("persistence: invalid store configuration")
)

// Config controls retention and backing storage.
//
// A zero size/count limit is disabled.  MaxArtifactBytes bounds one artifact;
// MaxTotalBytes bounds the sum of retained artifact bytes; MaxArtifacts bounds
// the number of retained references.  MaxMemoryBytes bounds bytes kept in
// process memory.  When an artifact does not fit the memory budget, SpillRoot
// is used if configured; without a spill root Put returns ErrMemoryLimit.
// Disk spill remains subject to MaxArtifactBytes, MaxTotalBytes, and
// MaxArtifacts.  TTL is based on last access and includes incomplete captures.
type Config struct {
	MaxArtifactBytes int64
	MaxTotalBytes    int64
	MaxMemoryBytes   int64
	MaxArtifacts     int

	// SpillRoot is an optional directory for large artifacts.  Files are
	// created with mode 0600 and names are derived only from validated opaque
	// artifact IDs.  The incoming wire.StorageRef is never treated as a path.
	SpillRoot string

	// Root is a compatibility spelling for SpillRoot.  If both are supplied
	// they must identify the same cleaned path.
	Root string

	// TTL of zero disables expiration.  Cleanup removes entries whose last
	// access is at least TTL old.  CleanupInterval optionally starts a bounded
	// janitor; manual Cleanup is always available.
	TTL             time.Duration
	CleanupInterval time.Duration
	Clock           func() time.Time
}

// Stats is a metadata-only store diagnostic.  It contains no body bytes.
type Stats struct {
	Artifacts   int
	Bytes       int64
	MemoryBytes int64
	DiskBytes   int64
}

// Store retains immutable artifact references and either an in-memory copy or
// a private disk-spill file.  Entries are never replaced: callers must create
// a new artifact ID for an edit/derived body.
type Store struct {
	mu sync.Mutex

	cfg     Config
	entries map[string]*entry

	usedBytes   int64
	memoryBytes int64

	closed      bool
	closeOnce   sync.Once
	janitorStop chan struct{}
	janitorDone chan struct{}
}

type entry struct {
	ref         wire.ArtifactRef
	data        []byte
	path        string
	createdAt   time.Time
	lastAccess  time.Time
	activeReads int
	removed     bool
}

// NewArtifactStore constructs an artifact store.  It creates SpillRoot when
// configured, but does not scan or adopt pre-existing files: only artifacts
// successfully Put during this process are addressable.  This avoids ever
// treating an unindexed file as an original artifact.
func NewArtifactStore(cfg Config) (*Store, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}

	s := &Store{
		cfg:     normalized,
		entries: make(map[string]*entry),
	}
	if normalized.CleanupInterval > 0 && normalized.TTL > 0 {
		s.janitorStop = make(chan struct{})
		s.janitorDone = make(chan struct{})
		go s.runJanitor(normalized.CleanupInterval)
	}
	return s, nil
}

// NewStore is a concise constructor alias.
func NewStore(cfg Config) (*Store, error) { return NewArtifactStore(cfg) }

func normalizeConfig(cfg Config) (Config, error) {
	if cfg.MaxArtifactBytes < 0 || cfg.MaxTotalBytes < 0 || cfg.MaxMemoryBytes < 0 || cfg.MaxArtifacts < 0 {
		return Config{}, fmt.Errorf("%w: size and count limits cannot be negative", ErrInvalidConfig)
	}
	if cfg.TTL < 0 || cfg.CleanupInterval < 0 {
		return Config{}, fmt.Errorf("%w: retention durations cannot be negative", ErrInvalidConfig)
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}

	if cfg.SpillRoot != "" && cfg.Root != "" {
		spill, err := filepath.Abs(filepath.Clean(cfg.SpillRoot))
		if err != nil {
			return Config{}, fmt.Errorf("%w: spill root: %v", ErrInvalidConfig, err)
		}
		root, err := filepath.Abs(filepath.Clean(cfg.Root))
		if err != nil {
			return Config{}, fmt.Errorf("%w: root: %v", ErrInvalidConfig, err)
		}
		if spill != root {
			return Config{}, fmt.Errorf("%w: SpillRoot and Root disagree", ErrInvalidConfig)
		}
	}
	if cfg.SpillRoot == "" {
		cfg.SpillRoot = cfg.Root
	}
	if cfg.SpillRoot != "" {
		root, err := filepath.Abs(filepath.Clean(cfg.SpillRoot))
		if err != nil {
			return Config{}, fmt.Errorf("%w: spill root: %v", ErrInvalidConfig, err)
		}
		if root == string(filepath.Separator) {
			return Config{}, fmt.Errorf("%w: spill root must not be filesystem root", ErrInvalidConfig)
		}
		if err := os.MkdirAll(root, defaultRootMode); err != nil {
			return Config{}, fmt.Errorf("%w: create spill root: %v", ErrInvalidConfig, err)
		}
		info, err := os.Stat(root)
		if err != nil {
			return Config{}, fmt.Errorf("%w: stat spill root: %v", ErrInvalidConfig, err)
		}
		if !info.IsDir() {
			return Config{}, fmt.Errorf("%w: spill root is not a directory", ErrInvalidConfig)
		}
		cfg.SpillRoot = root
	}
	return cfg, nil
}

func (s *Store) now() time.Time {
	if s == nil || s.cfg.Clock == nil {
		return time.Now().UTC()
	}
	return s.cfg.Clock()
}

// Put stores an immutable copy of artifact and returns metadata suitable for
// snapshots/events.  It rejects malformed or corrupted artifacts and never
// overwrites an existing artifact ID.  The incoming StorageRef is metadata
// only; disk paths are derived from ArtifactID after strict validation.
func (s *Store) Put(ctx context.Context, artifact wire.BodyArtifact) (wire.ArtifactRef, error) {
	if err := checkContext(ctx); err != nil {
		return wire.ArtifactRef{}, err
	}
	if s == nil {
		return wire.ArtifactRef{}, ErrClosed
	}
	ref := artifact.Ref()
	if err := ref.Validate(); err != nil {
		return wire.ArtifactRef{}, fmt.Errorf("%w: metadata validation failed", ErrInvalidArtifact)
	}
	if err := ValidateArtifactID(ref.ArtifactID); err != nil {
		return wire.ArtifactRef{}, err
	}
	if !artifact.Verify() {
		return wire.ArtifactRef{}, ErrInvalidArtifact
	}
	body := artifact.Bytes()
	if err := checkContext(ctx); err != nil {
		return wire.ArtifactRef{}, err
	}
	if int64(len(body)) != ref.Size {
		return wire.ArtifactRef{}, ErrInvalidArtifact
	}
	if s.cfg.MaxArtifactBytes > 0 && ref.Size > s.cfg.MaxArtifactBytes {
		return wire.ArtifactRef{}, ErrArtifactTooLarge
	}

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return wire.ArtifactRef{}, ErrClosed
	}
	if _, exists := s.entries[ref.ArtifactID]; exists {
		return wire.ArtifactRef{}, ErrArtifactExists
	}
	if s.cfg.MaxArtifacts > 0 && len(s.entries) >= s.cfg.MaxArtifacts {
		return wire.ArtifactRef{}, ErrStoreFull
	}
	if s.cfg.MaxTotalBytes > 0 && (ref.Size > s.cfg.MaxTotalBytes || s.usedBytes > s.cfg.MaxTotalBytes-ref.Size) {
		return wire.ArtifactRef{}, ErrStoreFull
	}

	keepMemory := true
	if s.cfg.MaxMemoryBytes > 0 && (ref.Size > s.cfg.MaxMemoryBytes || s.memoryBytes > s.cfg.MaxMemoryBytes-ref.Size) {
		keepMemory = false
		if s.cfg.SpillRoot == "" {
			return wire.ArtifactRef{}, ErrMemoryLimit
		}
	}

	e := &entry{ref: ref, createdAt: now, lastAccess: now}
	if keepMemory {
		// artifact.Bytes already returns a copy.  Keep this private copy and
		// never return it directly to callers.
		e.data = body
		s.memoryBytes += ref.Size
	} else {
		path, err := blobPath(s.cfg.SpillRoot, ref.ArtifactID)
		if err != nil {
			return wire.ArtifactRef{}, err
		}
		if err := writeBlob(ctx, path, body); err != nil {
			return wire.ArtifactRef{}, err
		}
		e.path = path
	}
	s.entries[ref.ArtifactID] = e
	s.usedBytes += ref.Size
	return ref, nil
}

// PutArtifact is the context-free convenience form of Put.
func (s *Store) PutArtifact(artifact wire.BodyArtifact) (wire.ArtifactRef, error) {
	return s.Put(context.Background(), artifact)
}

// Save is an alias for PutArtifact.
func (s *Store) Save(artifact wire.BodyArtifact) (wire.ArtifactRef, error) {
	return s.PutArtifact(artifact)
}

// Get returns a fresh immutable BodyArtifact containing the exact stored bytes.
// Incomplete capture state is retained in the returned reference; a partial
// blob is never silently upgraded to Complete=true.
func (s *Store) Get(ctx context.Context, artifactID string) (wire.BodyArtifact, error) {
	if err := checkContext(ctx); err != nil {
		return wire.BodyArtifact{}, err
	}
	reader, ref, err := s.Open(ctx, artifactID)
	if err != nil {
		return wire.BodyArtifact{}, err
	}
	defer reader.Close()

	body, err := readAllContext(ctx, reader, ref.Size)
	if err != nil {
		return wire.BodyArtifact{}, err
	}
	if err := checkContext(ctx); err != nil {
		return wire.BodyArtifact{}, err
	}
	artifact, err := wire.NewArtifactFromRef(ref, body)
	if err != nil {
		return wire.BodyArtifact{}, fmt.Errorf("%w: integrity check failed", ErrCorruptArtifact)
	}
	return artifact, nil
}

// GetArtifact is the context-free convenience form of Get.
func (s *Store) GetArtifact(artifactID string) (wire.BodyArtifact, error) {
	return s.Get(context.Background(), artifactID)
}

// Open returns a fresh reader and metadata.  The reader verifies size and
// SHA-256 as it reaches EOF, while allowing callers to stop early for a range
// or preview.  Closing always releases the store's reader lease.
func (s *Store) Open(ctx context.Context, artifactID string) (io.ReadCloser, wire.ArtifactRef, error) {
	if err := checkContext(ctx); err != nil {
		return nil, wire.ArtifactRef{}, err
	}
	if err := ValidateArtifactID(artifactID); err != nil {
		return nil, wire.ArtifactRef{}, err
	}
	if s == nil {
		return nil, wire.ArtifactRef{}, ErrClosed
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, wire.ArtifactRef{}, ErrClosed
	}
	e, ok := s.entries[artifactID]
	if !ok || e.removed {
		s.mu.Unlock()
		return nil, wire.ArtifactRef{}, ErrNotFound
	}
	e.activeReads++
	e.lastAccess = s.now()
	ref := e.ref
	var src io.Reader
	var closeFn io.Closer
	if e.path != "" {
		file, err := os.Open(e.path)
		if err != nil {
			e.activeReads--
			s.mu.Unlock()
			if errors.Is(err, os.ErrNotExist) {
				return nil, wire.ArtifactRef{}, ErrCorruptArtifact
			}
			return nil, wire.ArtifactRef{}, fmt.Errorf("%w: open backing blob", ErrCorruptArtifact)
		}
		src, closeFn = file, file
	} else {
		src = bytes.NewReader(e.data)
		// The bytes reader is private to the entry; callers receive only a
		// reader over it and can never mutate the store's backing slice.
		closeFn = io.NopCloser(nil)
	}
	s.mu.Unlock()

	if err := checkContext(ctx); err != nil {
		_ = closeFn.Close()
		s.releaseReader(e)
		return nil, wire.ArtifactRef{}, err
	}
	verified := &verifyingReader{src: src, expectedSize: ref.Size, expectedSHA256: ref.SHA256, hash: newHashState()}
	return &trackedReadCloser{
		reader:  verified,
		closer:  closeFn,
		release: func() { s.releaseReader(e) },
	}, ref, nil
}

// OpenArtifact is the context-free convenience form of Open.
func (s *Store) OpenArtifact(artifactID string) (io.ReadCloser, wire.ArtifactRef, error) {
	return s.Open(context.Background(), artifactID)
}

// Metadata returns a value copy of metadata without loading body bytes.  It is
// the preferred method for snapshots and event payload construction.
func (s *Store) Metadata(ctx context.Context, artifactID string) (wire.ArtifactRef, error) {
	if err := checkContext(ctx); err != nil {
		return wire.ArtifactRef{}, err
	}
	if err := ValidateArtifactID(artifactID); err != nil {
		return wire.ArtifactRef{}, err
	}
	if s == nil {
		return wire.ArtifactRef{}, ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return wire.ArtifactRef{}, ErrClosed
	}
	e, ok := s.entries[artifactID]
	if !ok || e.removed {
		return wire.ArtifactRef{}, ErrNotFound
	}
	e.lastAccess = s.now()
	return e.ref, nil
}

// ArtifactRef implements a metadata-only lookup named after the wire DTO.
func (s *Store) ArtifactRef(ctx context.Context, artifactID string) (wire.ArtifactRef, error) {
	return s.Metadata(ctx, artifactID)
}

// Ref is a concise metadata lookup alias.
func (s *Store) Ref(ctx context.Context, artifactID string) (wire.ArtifactRef, error) {
	return s.Metadata(ctx, artifactID)
}

// List returns sorted metadata-only references.  No body bytes are included.
func (s *Store) List(ctx context.Context) ([]wire.ArtifactRef, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	ids := make([]string, 0, len(s.entries))
	for id, e := range s.entries {
		if !e.removed {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	refs := make([]wire.ArtifactRef, 0, len(ids))
	for _, id := range ids {
		e := s.entries[id]
		e.lastAccess = s.now()
		refs = append(refs, e.ref)
	}
	return refs, nil
}

// ReadRange reads [start,end) without changing bytes.  It is useful for
// previews/download endpoints so they need not load a complete large blob.
func (s *Store) ReadRange(ctx context.Context, artifactID string, start, end int64) ([]byte, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	ref, err := s.Metadata(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	if start < 0 || end < start || end > ref.Size {
		return nil, ErrInvalidRange
	}
	length := end - start
	if uint64(length) > uint64(maxInt()) {
		return nil, ErrInvalidRange
	}
	reader, openedRef, err := s.Open(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	if openedRef.Size != ref.Size || openedRef.SHA256 != ref.SHA256 {
		return nil, ErrCorruptArtifact
	}
	if err := discardContext(ctx, reader, start); err != nil {
		return nil, err
	}
	out := make([]byte, int(length))
	if _, err := readFullContext(ctx, reader, out); err != nil {
		return nil, err
	}
	// When reading through the end, force the verifying reader to observe EOF
	// so a truncated or tampered backing blob cannot look like a valid range.
	if end == ref.Size {
		var probe [1]byte
		n, probeErr := reader.Read(probe[:])
		if n != 0 {
			return nil, ErrCorruptArtifact
		}
		if !errors.Is(probeErr, io.EOF) {
			if probeErr == nil {
				return nil, ErrCorruptArtifact
			}
			return nil, probeErr
		}
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes an artifact reference.  Active readers retain their open
// bytes and trigger backing-file removal once closed.
func (s *Store) Delete(ctx context.Context, artifactID string) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := ValidateArtifactID(artifactID); err != nil {
		return err
	}
	if s == nil {
		return ErrClosed
	}
	var path string
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	e, ok := s.entries[artifactID]
	if !ok || e.removed {
		s.mu.Unlock()
		return ErrNotFound
	}
	delete(s.entries, artifactID)
	e.removed = true
	s.usedBytes -= e.ref.Size
	if e.data != nil {
		s.memoryBytes -= e.ref.Size
		e.data = nil
	}
	if e.activeReads == 0 {
		path = e.path
		e.path = ""
	}
	s.mu.Unlock()
	return removeBackingFile(path)
}

// Cleanup removes entries whose last access is at least TTL old.  Incomplete
// artifacts are retained subject to the same TTL as complete artifacts; their
// Complete=false status is never changed by cleanup.
func (s *Store) Cleanup(ctx context.Context) (int, error) {
	if err := checkContext(ctx); err != nil {
		return 0, err
	}
	if s == nil {
		return 0, ErrClosed
	}
	now := s.now()
	var paths []string
	removed := 0
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrClosed
	}
	if s.cfg.TTL <= 0 {
		s.mu.Unlock()
		return 0, nil
	}
	for id, e := range s.entries {
		if e.removed || e.activeReads > 0 || !expired(now, e.lastAccess, s.cfg.TTL) {
			continue
		}
		delete(s.entries, id)
		e.removed = true
		removed++
		s.usedBytes -= e.ref.Size
		if e.data != nil {
			s.memoryBytes -= e.ref.Size
			e.data = nil
		}
		if e.activeReads == 0 {
			if e.path != "" {
				paths = append(paths, e.path)
				e.path = ""
			}
		}
	}
	s.mu.Unlock()
	return removed, removeBackingFiles(paths)
}

// CleanupExpired is a descriptive alias for Cleanup.
func (s *Store) CleanupExpired(ctx context.Context) (int, error) {
	return s.Cleanup(ctx)
}

// Stats returns metadata-only accounting.  Bytes includes both memory and
// disk entries and counts incomplete captures as retained bytes.
func (s *Store) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Artifacts:   len(s.entries),
		Bytes:       s.usedBytes,
		MemoryBytes: s.memoryBytes,
		DiskBytes:   s.usedBytes - s.memoryBytes,
	}
}

// Close makes the store unusable and removes indexed spill files.  Open
// readers remain usable until they are closed; on platforms where an open
// file cannot be unlinked, their release callback performs the final removal.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var paths []string
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		for id, e := range s.entries {
			delete(s.entries, id)
			e.removed = true
			if e.data != nil {
				e.data = nil
			}
			if e.activeReads == 0 && e.path != "" {
				paths = append(paths, e.path)
				e.path = ""
			}
		}
		s.usedBytes = 0
		s.memoryBytes = 0
		stop, done := s.janitorStop, s.janitorDone
		s.mu.Unlock()
		if stop != nil {
			close(stop)
			<-done
		}
	})
	return removeBackingFiles(paths)
}

func (s *Store) runJanitor(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer close(s.janitorDone)
	for {
		select {
		case <-ticker.C:
			_, _ = s.Cleanup(context.Background())
		case <-s.janitorStop:
			return
		}
	}
}

func (s *Store) releaseReader(e *entry) {
	if s == nil || e == nil {
		return
	}
	var path string
	s.mu.Lock()
	if e.activeReads > 0 {
		e.activeReads--
	}
	if e.removed && e.activeReads == 0 && e.path != "" {
		path = e.path
		e.path = ""
	}
	s.mu.Unlock()
	_ = removeBackingFile(path)
}

func expired(now, last time.Time, ttl time.Duration) bool {
	if ttl <= 0 || now.Before(last) {
		return false
	}
	return now.Sub(last) >= ttl
}

// ValidateArtifactID rejects IDs that could become absolute paths, traversal
// components, or ambiguous filesystem names.  Generated wire IDs satisfy this
// grammar.  StorageRef is deliberately not interpreted as a filesystem path.
func ValidateArtifactID(id string) error {
	if id == "" || len(id) > 255 || id == "." || id == ".." || strings.Contains(id, "..") {
		return ErrInvalidArtifactID
	}
	if !utf8.ValidString(id) || filepath.IsAbs(id) || strings.ContainsAny(id, `/\\`) {
		return ErrInvalidArtifactID
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return ErrInvalidArtifactID
	}
	return nil
}

func blobPath(root, id string) (string, error) {
	if root == "" {
		return "", ErrMemoryLimit
	}
	if err := ValidateArtifactID(id); err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("%w: resolve spill root", ErrInvalidConfig)
	}
	path := filepath.Join(rootAbs, id+".blob")
	rel, err := filepath.Rel(rootAbs, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", ErrInvalidArtifactID
	}
	return path, nil
}

func writeBlob(ctx context.Context, path string, body []byte) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, defaultFileMode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrArtifactExists
		}
		return fmt.Errorf("persistence: create spill blob: %w", err)
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	for offset := 0; offset < len(body); {
		if err := checkContext(ctx); err != nil {
			return err
		}
		n, writeErr := file.Write(body[offset:])
		if n > 0 {
			offset += n
		}
		if writeErr != nil {
			return fmt.Errorf("persistence: write spill blob: %w", writeErr)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("persistence: sync spill blob: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("persistence: close spill blob: %w", err)
	}
	ok = true
	return nil
}

func removeBackingFile(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("persistence: remove spill blob: %w", err)
	}
	return nil
}

func removeBackingFiles(paths []string) error {
	var first error
	for _, path := range paths {
		if err := removeBackingFile(path); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func readAllContext(ctx context.Context, r io.Reader, expected int64) ([]byte, error) {
	if expected < 0 || uint64(expected) > uint64(maxInt()) {
		return nil, ErrCorruptArtifact
	}
	out := make([]byte, 0, int(expected))
	chunk := make([]byte, 32*1024)
	for {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		n, err := r.Read(chunk)
		if n > 0 {
			out = append(out, chunk[:n]...)
			if int64(len(out)) > expected {
				return nil, ErrCorruptArtifact
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if int64(len(out)) != expected {
					return nil, ErrCorruptArtifact
				}
				return out, nil
			}
			return nil, err
		}
	}
}

func discardContext(ctx context.Context, r io.Reader, count int64) error {
	buf := make([]byte, 32*1024)
	for count > 0 {
		if err := checkContext(ctx); err != nil {
			return err
		}
		want := int64(len(buf))
		if count < want {
			want = count
		}
		n, err := r.Read(buf[:int(want)])
		if n > 0 {
			count -= int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) && count > 0 {
				return ErrCorruptArtifact
			}
			if !errors.Is(err, io.EOF) {
				return err
			}
		}
		if n == 0 && err == nil {
			return io.ErrNoProgress
		}
	}
	return nil
}

func readFullContext(ctx context.Context, r io.Reader, out []byte) (int, error) {
	off := 0
	for off < len(out) {
		if err := checkContext(ctx); err != nil {
			return off, err
		}
		n, err := r.Read(out[off:])
		if n > 0 {
			off += n
		}
		if err != nil {
			if errors.Is(err, io.EOF) && off < len(out) {
				return off, ErrCorruptArtifact
			}
			if !errors.Is(err, io.EOF) {
				return off, err
			}
		}
		if n == 0 && err == nil {
			return off, io.ErrNoProgress
		}
	}
	return off, nil
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

// verifyingReader performs integrity checks only when the complete stream is
// consumed.  This keeps Open useful for range/preview readers while making Get
// and full reads reject truncation, extension, or tampering.
type verifyingReader struct {
	src            io.Reader
	expectedSize   int64
	expectedSHA256 string
	count          int64
	hash           hashState
	done           bool
	terminalErr    error
}

// hashState is a streaming SHA-256 wrapper.  It does not retain body bytes,
// which keeps disk-spilled reads bounded by the caller's read buffer.
type hashState struct {
	h hash.Hash
}

func newHashState() hashState { return hashState{h: sha256.New()} }

func (h *hashState) Write(p []byte) {
	if h.h == nil {
		h.h = sha256.New()
	}
	_, _ = h.h.Write(p)
}

func (h *hashState) Sum() string {
	if h.h == nil {
		h.h = sha256.New()
	}
	return hex.EncodeToString(h.h.Sum(nil))
}

func (r *verifyingReader) Read(p []byte) (int, error) {
	if r.done {
		if r.terminalErr != nil {
			return 0, r.terminalErr
		}
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	remaining := r.expectedSize - r.count
	if remaining < 0 {
		r.done = true
		r.terminalErr = ErrCorruptArtifact
		return 0, r.terminalErr
	}
	if remaining == 0 {
		var probe [1]byte
		n, err := r.src.Read(probe[:])
		if n > 0 {
			r.done = true
			r.terminalErr = ErrCorruptArtifact
			return 0, r.terminalErr
		}
		if err == nil {
			return 0, nil
		}
		if errors.Is(err, io.EOF) {
			r.done = true
			if r.hash.Sum() != r.expectedSHA256 {
				r.terminalErr = ErrCorruptArtifact
				return 0, r.terminalErr
			}
			return 0, io.EOF
		}
		r.done = true
		r.terminalErr = err
		return 0, err
	}
	limit := len(p)
	if int64(limit) > remaining {
		limit = int(remaining)
	}
	n, err := r.src.Read(p[:limit])
	if n < 0 || n > limit {
		r.done = true
		r.terminalErr = ErrCorruptArtifact
		return 0, r.terminalErr
	}
	if n > 0 {
		r.count += int64(n)
		r.hash.Write(p[:n])
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			if r.count != r.expectedSize || r.hash.Sum() != r.expectedSHA256 {
				r.done = true
				r.terminalErr = ErrCorruptArtifact
				return n, r.terminalErr
			}
			// Return the bytes first.  The next read performs the EOF probe,
			// matching normal io.Reader behavior for n > 0.
			return n, nil
		}
		r.done = true
		r.terminalErr = err
		return n, err
	}
	return n, nil
}

// trackedReadCloser keeps a reader lease until EOF/error/Close.  It prevents
// cleanup from unlinking a spill file while a caller is actively reading it.
type trackedReadCloser struct {
	reader  io.Reader
	closer  io.Closer
	release func()
	once    sync.Once
}

func (r *trackedReadCloser) Read(p []byte) (int, error) {
	if r == nil || r.reader == nil {
		return 0, ErrClosed
	}
	n, err := r.reader.Read(p)
	if err != nil {
		r.releaseOnce()
	}
	return n, err
}

func (r *trackedReadCloser) Close() error {
	if r == nil {
		return nil
	}
	err := error(nil)
	if r.closer != nil {
		err = r.closer.Close()
	}
	r.releaseOnce()
	return err
}

func (r *trackedReadCloser) releaseOnce() {
	r.once.Do(func() {
		if r.release != nil {
			r.release()
		}
	})
}

var _ interface {
	Put(context.Context, wire.BodyArtifact) (wire.ArtifactRef, error)
	Get(context.Context, string) (wire.BodyArtifact, error)
	Open(context.Context, string) (io.ReadCloser, wire.ArtifactRef, error)
	ArtifactRef(context.Context, string) (wire.ArtifactRef, error)
} = (*Store)(nil)
