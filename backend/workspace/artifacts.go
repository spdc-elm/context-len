package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"context-lens/backend/wire"
)

var (
	// ErrArtifactNotFound is returned when an id is not present in a body
	// store.  HTTP handlers map it to 404 without exposing store internals.
	ErrArtifactNotFound = errors.New("workspace: artifact not found")
	// ErrArtifactExists prevents replacing an immutable body under an existing
	// id.  Derived edits must use a new artifact id.
	ErrArtifactExists = errors.New("workspace: artifact already exists")
	// ErrArtifactInvalid identifies a body whose reference does not match its
	// exact bytes.
	ErrArtifactInvalid = errors.New("workspace: invalid artifact")
	// ErrArtifactTooLarge is returned before body bytes cross the HTTP
	// boundary when a full response/range exceeds the configured limit.
	ErrArtifactTooLarge = errors.New("workspace: artifact exceeds configured body limit")
	// ErrArtifactRange identifies malformed or unsatisfiable byte ranges.
	ErrArtifactRange = errors.New("workspace: invalid artifact range")
	// Stable classes for adapters (including durable stores) to wrap without
	// exposing implementation details through the HTTP API.
	ErrArtifactQuota            = errors.New("workspace: artifact quota exceeded")
	ErrArtifactStoreUnavailable = errors.New("workspace: artifact store unavailable")
)

// MemoryArtifactStore is a small immutable in-memory blob store for the local
// MVP and tests.  It keeps exact application bytes and implements
// RangeArtifactStore so HTTP range/search requests do not copy a whole body.
type MemoryArtifactStore struct {
	mu       sync.RWMutex
	items    map[string]wire.BodyArtifact
	maxBytes int64
}

// NewMemoryArtifactStore creates a store.  maxBytes <= 0 means no insertion
// limit; HTTP response limits are still enforced by Server configuration.
func NewMemoryArtifactStore(maxBytes int64) *MemoryArtifactStore {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return &MemoryArtifactStore{items: make(map[string]wire.BodyArtifact), maxBytes: maxBytes}
}

// Put stores an immutable artifact.  The artifact's ref id, size and hash are
// checked before insertion.  Existing ids are rejected to preserve the
// original-artifact immutability invariant.
func (s *MemoryArtifactStore) Put(artifact wire.BodyArtifact) error {
	if s == nil {
		return errors.New("workspace: nil artifact store")
	}
	ref := artifact.Ref()
	if ref.ArtifactID == "" || ref.Size < 0 || ref.StorageRef == "" || len(ref.SHA256) != 64 {
		return ErrArtifactInvalid
	}
	body := artifact.Bytes()
	if !artifact.Verify() || int64(len(body)) != ref.Size {
		return ErrArtifactInvalid
	}
	if s.maxBytes > 0 && ref.Size > s.maxBytes {
		return fmt.Errorf("workspace: artifact exceeds store limit: %w", ErrArtifactInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[ref.ArtifactID]; exists {
		return ErrArtifactExists
	}
	s.items[ref.ArtifactID] = artifact
	return nil
}

// Add is an alias for Put.
func (s *MemoryArtifactStore) Add(artifact wire.BodyArtifact) error { return s.Put(artifact) }

// Get returns an immutable artifact value.  BodyArtifact.Bytes and Reader
// return independent copies/readers, so callers cannot mutate the store.
func (s *MemoryArtifactStore) Get(ctx context.Context, artifactID string) (wire.BodyArtifact, error) {
	if err := contextErr(ctx); err != nil {
		return wire.BodyArtifact{}, err
	}
	if s == nil {
		return wire.BodyArtifact{}, ErrArtifactNotFound
	}
	s.mu.RLock()
	artifact, ok := s.items[artifactID]
	s.mu.RUnlock()
	if !ok {
		return wire.BodyArtifact{}, ErrArtifactNotFound
	}
	if err := contextErr(ctx); err != nil {
		return wire.BodyArtifact{}, err
	}
	return artifact, nil
}

// ArtifactRef returns metadata without exposing body bytes.
func (s *MemoryArtifactStore) ArtifactRef(ctx context.Context, artifactID string) (wire.ArtifactRef, error) {
	a, err := s.Get(ctx, artifactID)
	if err != nil {
		return wire.ArtifactRef{}, err
	}
	return a.Ref(), nil
}

// ReadRange returns exact bytes for [start,end).  It validates bounds rather
// than silently clamping, which keeps Content-Range diagnostics unambiguous.
func (s *MemoryArtifactStore) ReadRange(ctx context.Context, artifactID string, start, end int64) ([]byte, error) {
	if start < 0 || end < start {
		return nil, fmt.Errorf("workspace: invalid artifact range: %w", ErrArtifactInvalid)
	}
	a, err := s.Get(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	body := a.Bytes()
	if end > int64(len(body)) || start > int64(len(body)) {
		return nil, fmt.Errorf("workspace: artifact range outside body: %w", ErrArtifactInvalid)
	}
	return append([]byte(nil), body[start:end]...), nil
}

// Search returns byte offsets for every occurrence of query, capped at limit.
// Empty queries produce no matches.  Overlapping occurrences are included,
// which is useful for binary/opaque body inspection.
func (s *MemoryArtifactStore) Search(ctx context.Context, artifactID string, query []byte, limit int) ([]ArtifactMatch, error) {
	if len(query) == 0 || limit == 0 {
		return []ArtifactMatch{}, nil
	}
	if limit < 0 {
		limit = 0
	}
	ref, err := s.ArtifactRef(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	const chunkSize int64 = 64 << 10
	matches := make([]ArtifactMatch, 0)
	var carry []byte
	for pos := int64(0); pos < ref.Size && len(matches) < limit; {
		end := pos + chunkSize
		if end > ref.Size {
			end = ref.Size
		}
		part, err := s.ReadRange(ctx, artifactID, pos, end)
		if err != nil {
			return nil, err
		}
		data := append(carry, part...)
		base := pos - int64(len(carry))
		for at := 0; at+len(query) <= len(data) && len(matches) < limit; {
			rel := bytes.Index(data[at:], query)
			if rel < 0 {
				break
			}
			start := at + rel
			if start+len(query) > len(carry) || pos == 0 {
				matches = append(matches, ArtifactMatch{Start: base + int64(start), End: base + int64(start+len(query))})
			}
			at = start + 1
		}
		keep := len(query) - 1
		if keep < 0 {
			keep = 0
		}
		if keep > len(data) {
			keep = len(data)
		}
		carry = append([]byte(nil), data[len(data)-keep:]...)
		pos = end
	}
	return matches, nil
}

// Clear removes all artifacts while leaving the store ready for new bodies.
func (s *MemoryArtifactStore) Clear(ctx context.Context) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if s == nil {
		return errors.New("workspace: nil artifact store")
	}
	s.mu.Lock()
	s.items = make(map[string]wire.BodyArtifact)
	s.mu.Unlock()
	return nil
}

// IDs returns the known ids in sorted order.  It is useful for diagnostics and
// does not expose body bytes.
func (s *MemoryArtifactStore) IDs() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	ids := make([]string, 0, len(s.items))
	for id := range s.items {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

var _ ArtifactStore = (*MemoryArtifactStore)(nil)
var _ RangeArtifactStore = (*MemoryArtifactStore)(nil)
