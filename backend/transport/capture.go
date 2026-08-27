package transport

import (
	"bytes"
	"errors"
	"io"
	"sync"

	"context-lens/backend/wire"
)

// CaptureReadCloser tees an opaque body stream into an in-memory byte buffer.
// The source bytes continue to flow to the caller unchanged; capture limits
// only bound the diagnostic copy and never truncate the upstream request or
// response.  A capture that ends before EOF is marked incomplete.
type CaptureReadCloser struct {
	source io.ReadCloser
	limit  int64

	mu        sync.Mutex
	buf       bytes.Buffer
	complete  bool
	closed    bool
	readErr   error
	truncated bool
}

// NewCaptureReadCloser wraps source.  A non-positive limit means unlimited.
// A nil source is represented by an empty, already-complete reader only when
// NewCaptureReadCloser is not used; nil is rejected to catch wiring mistakes.
func NewCaptureReadCloser(source io.ReadCloser, limit int64) (*CaptureReadCloser, error) {
	if source == nil {
		return nil, errors.New("transport: nil capture source")
	}
	return &CaptureReadCloser{source: source, limit: limit}, nil
}

// Read forwards exactly the bytes returned by source while retaining a copy
// up to the configured capture limit.  Source errors are remembered for
// Snapshot so observers can distinguish an incomplete body from a complete
// artifact.
func (c *CaptureReadCloser) Read(p []byte) (int, error) {
	if c == nil || c.source == nil {
		return 0, io.EOF
	}
	n, err := c.source.Read(p)
	if n > 0 {
		c.mu.Lock()
		if !c.truncated {
			if c.limit > 0 && int64(c.buf.Len())+int64(n) > c.limit {
				allowed := c.limit - int64(c.buf.Len())
				if allowed > 0 {
					_, _ = c.buf.Write(p[:int(allowed)])
				}
				c.truncated = true
			} else {
				_, _ = c.buf.Write(p[:n])
			}
		}
		c.mu.Unlock()
	}
	if err != nil {
		c.mu.Lock()
		c.readErr = err
		if errors.Is(err, io.EOF) && !c.truncated {
			c.complete = true
		}
		c.mu.Unlock()
	}
	return n, err
}

// Close closes the underlying source.  Closing before EOF leaves Complete
// false; this is important when a downstream disconnect aborts a stream.
func (c *CaptureReadCloser) Close() error {
	if c == nil || c.source == nil {
		return nil
	}
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.source.Close()
}

// Snapshot returns an independent copy of captured bytes and its status.
// Err is io.EOF for a complete source and the source's actual terminal error
// for an interrupted source; a body closed before any terminal read has a nil
// Err and Complete=false.
func (c *CaptureReadCloser) Snapshot() (body []byte, complete bool, err error) {
	if c == nil {
		return nil, false, errors.New("transport: nil capture")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	body = append([]byte(nil), c.buf.Bytes()...)
	complete = c.complete
	err = c.readErr
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if c.truncated && err == nil {
		err = ErrCaptureLimit
	}
	return body, complete, err
}

// ErrCaptureLimit reports that CaptureReadCloser retained only its configured
// prefix while allowing the complete source stream to continue downstream.
var ErrCaptureLimit = errors.New("transport: body capture limit exceeded")

type CapturedBody struct {
	Stage     string
	Direction string
	Artifact  wire.BodyArtifact
	Err       error
}

// CaptureFunc receives diagnostic body artifacts.  Implementations should be
// quick and non-blocking: capture must never hold up pass-through traffic.
type CaptureFunc func(CapturedBody)
