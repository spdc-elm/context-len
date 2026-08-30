package persistence

import (
	"bytes"
	"context"
	"context-lens/backend/wire"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type ArtifactRepository interface {
	Begin(context.Context, wire.ArtifactOptions) (*CaptureWriter, error)
	Open(context.Context, string) (io.ReadCloser, wire.ArtifactRef, error)
	ReadRange(context.Context, string, int64, int64) ([]byte, error)
	Search(context.Context, string, []byte, int) ([]wire.ArtifactMatch, error)
}
type CaptureWriter struct {
	store                         *Store
	file                          *os.File
	path                          string
	opts                          wire.ArtifactOptions
	h                             hash.Hash
	size                          int64
	mu                            sync.Mutex
	closed, committed, committing bool
	epoch                         uint64
}

func (s *Store) Begin(c context.Context, o wire.ArtifactOptions) (*CaptureWriter, error) {
	if e := checkContext(c); e != nil {
		return nil, e
	}
	if s == nil {
		return nil, ErrClosed
	}
	s.mu.Lock()
	closed := s.closed
	epoch := s.epoch
	s.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	r := s.cfg.SpillRoot
	if r == "" {
		r = os.TempDir()
	}
	d := filepath.Join(r, ".staging")
	if e := os.MkdirAll(d, defaultRootMode); e != nil {
		return nil, e
	}
	f, e := os.CreateTemp(d, "capture-")
	if e != nil {
		return nil, e
	}
	_ = f.Chmod(defaultFileMode)
	return &CaptureWriter{s, f, f.Name(), o, sha256.New(), 0, sync.Mutex{}, false, false, false, epoch}, nil
}
func (s *Store) BeginWriter(c context.Context, o wire.ArtifactOptions) (*CaptureWriter, error) {
	return s.Begin(c, o)
}
func (w *CaptureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.committed || w.committing {
		return 0, ErrClosed
	}
	if w.store.cfg.MaxArtifactBytes > 0 && w.size+int64(len(p)) > w.store.cfg.MaxArtifactBytes {
		return 0, ErrArtifactTooLarge
	}
	n, e := w.file.Write(p)
	if n > 0 {
		_, _ = w.h.Write(p[:n])
		w.size += int64(n)
	}
	return n, e
}
func (w *CaptureWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.committed {
		return nil
	}
	w.closed = true
	w.committing = false
	_ = w.file.Close()
	return os.Remove(w.path)
}
func (w *CaptureWriter) Commit(c context.Context, complete bool) (wire.ArtifactRef, error) {
	w.mu.Lock()
	if w.closed || w.committed || w.committing {
		w.mu.Unlock()
		return wire.ArtifactRef{}, ErrClosed
	}
	if e := checkContext(c); e != nil {
		w.closed = true
		_ = w.file.Close()
		_ = os.Remove(w.path)
		w.mu.Unlock()
		return wire.ArtifactRef{}, e
	}
	if e := w.file.Sync(); e != nil {
		w.closed = true
		_ = w.file.Close()
		_ = os.Remove(w.path)
		w.mu.Unlock()
		return wire.ArtifactRef{}, e
	}
	if e := w.file.Close(); e != nil {
		w.closed = true
		_ = os.Remove(w.path)
		w.mu.Unlock()
		return wire.ArtifactRef{}, e
	}
	w.closed = true
	w.committing = true
	sum := hex.EncodeToString(w.h.Sum(nil))
	size := w.size
	o := w.opts
	w.mu.Unlock()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(w.path)
			w.mu.Lock()
			w.committing = false
			w.mu.Unlock()
		}
	}()
	if o.ArtifactID == "" {
		var b [16]byte
		if _, e := rand.Read(b[:]); e != nil {
			return wire.ArtifactRef{}, e
		}
		o.ArtifactID = hex.EncodeToString(b[:])
	}
	if e := ValidateArtifactID(o.ArtifactID); e != nil {
		_ = os.Remove(w.path)
		return wire.ArtifactRef{}, e
	}
	key := fmt.Sprintf("%s-%d", sum, size)
	ref := wire.ArtifactRef{ArtifactID: o.ArtifactID, Stage: o.Stage, Direction: o.Direction, ContentType: o.ContentType, ContentEncoding: o.ContentEncoding, Size: size, SHA256: sum, Complete: complete, StorageRef: key}
	if e := ref.Validate(); e != nil {
		_ = os.Remove(w.path)
		return wire.ArtifactRef{}, e
	}
	s := w.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = os.Remove(w.path)
		return wire.ArtifactRef{}, ErrClosed
	}
	if s.epoch != w.epoch {
		_ = os.Remove(w.path)
		return wire.ArtifactRef{}, ErrStaleArtifact
	}
	if _, ok := s.entries[ref.ArtifactID]; ok {
		_ = os.Remove(w.path)
		return wire.ArtifactRef{}, ErrArtifactExists
	}
	if s.cfg.MaxArtifacts > 0 && len(s.entries) >= s.cfg.MaxArtifacts {
		_ = os.Remove(w.path)
		return wire.ArtifactRef{}, ErrStoreFull
	}
	b := s.blobs[key]
	if b == nil {
		if s.cfg.MaxTotalBytes > 0 && (size > s.cfg.MaxTotalBytes || s.usedBytes > s.cfg.MaxTotalBytes-size) {
			return wire.ArtifactRef{}, ErrStoreFull
		}
		root := s.cfg.SpillRoot
		if root == "" {
			root = filepath.Dir(w.path)
		}
		d := filepath.Join(root, "blobs", sum[:2], sum[2:4])
		if e := os.MkdirAll(d, defaultRootMode); e != nil {
			return wire.ArtifactRef{}, e
		}
		p := filepath.Join(d, key+".blob")
		if e := os.Rename(w.path, p); e != nil {
			if _, st := os.Stat(p); st != nil {
				return wire.ArtifactRef{}, e
			}
		}
		b = &blobRecord{key: key, path: p, size: size}
		s.blobs[key] = b
		s.usedBytes += size
	} else {
		_ = os.Remove(w.path)
	}
	b.refs++
	now := s.now()
	s.entries[ref.ArtifactID] = &entry{ref: ref, blob: b, path: b.path, createdAt: now, lastAccess: now}
	w.mu.Lock()
	w.committed = true
	w.committing = false
	w.mu.Unlock()
	return ref, nil
}

// SearchMatch is retained as a compatibility alias. New code should use
// wire.ArtifactMatch so artifact search crosses package boundaries cleanly.
type SearchMatch = wire.ArtifactMatch

func (s *Store) Search(c context.Context, id string, q []byte, limit int) ([]wire.ArtifactMatch, error) {
	if len(q) == 0 || limit == 0 {
		return nil, nil
	}
	if limit < 0 {
		limit = 1 << 30
	}
	r, _, e := s.Open(c, id)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	buf := make([]byte, 32768+len(q)-1)
	var base int64
	carry := 0
	out := []wire.ArtifactMatch{}
	for {
		n, er := r.Read(buf[carry:])
		n += carry
		start := base - int64(carry)
		for i := 0; i+len(q) <= n && len(out) < limit; i++ {
			if bytes.Equal(buf[i:i+len(q)], q) {
				out = append(out, wire.ArtifactMatch{Start: start + int64(i), End: start + int64(i+len(q))})
			}
		}
		base += int64(n - carry)
		carry = n
		if carry > len(q)-1 {
			carry = len(q) - 1
		}
		copy(buf[:carry], buf[n-carry:n])
		if er == io.EOF {
			break
		}
		if er != nil {
			return nil, er
		}
	}
	return out, nil
}
func (s *Store) Lazy(c context.Context, id string) (wire.BodyArtifact, error) {
	r, e := s.Metadata(c, id)
	if e != nil {
		return wire.BodyArtifact{}, e
	}
	return wire.NewLazyArtifact(r, func() (io.ReadCloser, error) { x, _, e := s.Open(context.Background(), id); return x, e })
}
func (s *Store) Link(c context.Context, srcID string, o wire.ArtifactOptions) (wire.ArtifactRef, error) {
	if e := checkContext(c); e != nil {
		return wire.ArtifactRef{}, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src, ok := s.entries[srcID]
	if !ok || src.blob == nil {
		return wire.ArtifactRef{}, ErrNotFound
	}
	if s.cfg.MaxArtifacts > 0 && len(s.entries) >= s.cfg.MaxArtifacts {
		return wire.ArtifactRef{}, ErrStoreFull
	}
	id := o.ArtifactID
	if id == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		id = hex.EncodeToString(b[:])
	}
	if _, ok := s.entries[id]; ok {
		return wire.ArtifactRef{}, ErrArtifactExists
	}
	r := src.ref
	r.ArtifactID = id
	r.Stage = o.Stage
	r.Direction = o.Direction
	r.StorageRef = src.blob.key
	src.blob.refs++
	now := s.now()
	s.entries[id] = &entry{ref: r, blob: src.blob, path: src.blob.path, createdAt: now, lastAccess: now}
	// Links share the physical blob; only the logical reference count changes.
	return r, nil
}
func LazyRef(r wire.ArtifactRef, o func() (io.ReadCloser, error)) (wire.BodyArtifact, error) {
	return wire.NewLazyArtifact(r, o)
}
