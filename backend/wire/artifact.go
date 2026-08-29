// Package wire contains the application-layer wire values used by the proxy.
//
// A body artifact is the authority for bytes sent to, or received from, an
// upstream.  The inspection and UI layers may derive projections from a copy,
// but they must not replace an artifact with a decoded and re-encoded value.
package wire

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// ArtifactRef is the serializable reference to a body blob.  Body bytes are
// kept separately from this value; storage_ref identifies the backing store
// entry and artifact_id is the stable identity used by the workspace API.
//
// Original artifacts are immutable by convention.  An edit must make a new
// reference rather than changing the reference or bytes of the original.
type ArtifactRef struct {
	ArtifactID      string `json:"artifact_id"`
	Stage           string `json:"stage"`
	Direction       string `json:"direction"`
	ContentType     string `json:"content_type"`
	ContentEncoding string `json:"content_encoding"`
	Size            int64  `json:"size"`
	SHA256          string `json:"sha256"`
	Complete        bool   `json:"complete"`
	StorageRef      string `json:"storage_ref"`
}

// Well-known artifact stages.  Stage remains a string in ArtifactRef so that
// future stages can be represented without changing the public DTO.
const (
	StageRequestInbound     = "request.inbound"
	StageRequestUpstream    = "request.upstream"
	StageResponseUpstream   = "response.upstream"
	StageResponseDownstream = "response.downstream"
)

// Well-known artifact directions.  Direction remains a string in ArtifactRef
// for forward compatibility with additional capture directions.
const (
	DirectionInbound    = "inbound"
	DirectionUpstream   = "upstream"
	DirectionDownstream = "downstream"
)

// CaptureStatus describes the status represented by ArtifactRef.Complete.  It
// is intentionally not another JSON field: complete is the frozen wire DTO,
// while this type gives callers a readable status without changing that DTO.
type CaptureStatus string

const (
	CaptureComplete   CaptureStatus = "complete"
	CaptureIncomplete CaptureStatus = "capture_incomplete"
)

// Status returns the capture status represented by the reference.
func (r ArtifactRef) Status() CaptureStatus {
	if r.Complete {
		return CaptureComplete
	}
	return CaptureIncomplete
}

// ArtifactOptions supplies metadata for a newly-created artifact.  An empty
// ArtifactID is replaced with a locally generated opaque id.  An empty
// StorageRef defaults to the resulting ArtifactID, which is suitable for an
// in-memory store and can be replaced by a persistence layer later.
type ArtifactOptions struct {
	ArtifactID      string
	Stage           string
	Direction       string
	ContentType     string
	ContentEncoding string
	StorageRef      string
}

// NewArtifact creates a complete in-memory body artifact.  The input bytes
// are copied before returning, and Bytes returns another copy, so callers
// cannot mutate the authority through either slice.  SHA256 and Size are
// computed over exactly those bytes; no parsing or serialization is involved.
func NewArtifact(body []byte, opts ArtifactOptions) BodyArtifact {
	return newBodyArtifact(body, opts, true)
}

// NewIncompleteArtifact creates an artifact for a partial capture.  Its hash
// and size describe the bytes that were captured, while Complete is false so
// consumers never mistake the prefix for a complete upstream/downstream body.
func NewIncompleteArtifact(body []byte, opts ArtifactOptions) BodyArtifact {
	return newBodyArtifact(body, opts, false)
}

// NewArtifactWithComplete creates an artifact while explicitly selecting its
// capture status.  It is useful to turn a capture result into an immutable
// value without duplicating metadata construction.
func NewArtifactWithComplete(body []byte, opts ArtifactOptions, complete bool) BodyArtifact {
	return newBodyArtifact(body, opts, complete)
}

// NewArtifactRef creates only the serializable reference for body.  It uses
// the same metadata and hashing rules as NewArtifact.  The body itself is not
// retained by the returned value.
func NewArtifactRef(body []byte, opts ArtifactOptions) ArtifactRef {
	return newArtifactRef(body, opts, true)
}

// NewIncompleteArtifactRef creates only the serializable reference for a
// partial capture.  SHA256 is the digest of the captured bytes and Complete is
// false.
func NewIncompleteArtifactRef(body []byte, opts ArtifactOptions) ArtifactRef {
	return newArtifactRef(body, opts, false)
}

// NewArtifactRefWithComplete creates only a reference with an explicit
// complete/capture status.
func NewArtifactRefWithComplete(body []byte, opts ArtifactOptions, complete bool) ArtifactRef {
	return newArtifactRef(body, opts, complete)
}

func newBodyArtifact(body []byte, opts ArtifactOptions, complete bool) BodyArtifact {
	copied := cloneBytes(body)
	return BodyArtifact{
		ref:  newArtifactRef(copied, opts, complete),
		body: copied,
	}
}

func newArtifactRef(body []byte, opts ArtifactOptions, complete bool) ArtifactRef {
	id := opts.ArtifactID
	if id == "" {
		id = newArtifactID()
	}
	storageRef := opts.StorageRef
	if storageRef == "" {
		storageRef = id
	}
	return ArtifactRef{
		ArtifactID:      id,
		Stage:           opts.Stage,
		Direction:       opts.Direction,
		ContentType:     opts.ContentType,
		ContentEncoding: opts.ContentEncoding,
		Size:            int64(len(body)),
		SHA256:          SHA256Hex(body),
		Complete:        complete,
		StorageRef:      storageRef,
	}
}

var generatedIDCounter atomic.Uint64

// newArtifactID deliberately has no semantic content.  Artifact ids may be
// exposed to a browser and should not contain request bytes, credentials, or
// filesystem paths.
func newArtifactID() string {
	var random [16]byte
	if _, err := io.ReadFull(rand.Reader, random[:]); err == nil {
		return hex.EncodeToString(random[:])
	}
	// crypto/rand failure is exceptionally unlikely.  Keep the primitive
	// usable in constrained environments without exposing body content.
	seq := generatedIDCounter.Add(1)
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), seq)
}

// SHA256Hex returns the lowercase hexadecimal SHA-256 digest of body.
func SHA256Hex(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

// BodyArtifact owns an immutable-ish in-memory copy of an application body
// and its reference metadata.  A persistence-backed implementation can keep
// only the reference and provide the same Reader contract; this primitive is
// intentionally small and deterministic for the local MVP and tests.
type BodyArtifact struct {
	ref  ArtifactRef
	body []byte
}

// Ref returns a value copy of the artifact metadata.  Mutating the returned
// value cannot alter this artifact.
func (a BodyArtifact) Ref() ArtifactRef { return a.ref }

// ArtifactRef is an explicit alias for Ref for callers that prefer the DTO's
// name when wiring exchange snapshots.
func (a BodyArtifact) ArtifactRef() ArtifactRef { return a.ref }

// Bytes returns a copy of the exact application bytes.  It never decodes or
// re-encodes JSON, SSE, or any other protocol body.
func (a BodyArtifact) Bytes() []byte { return cloneBytes(a.body) }

// Reader returns a fresh reader positioned at byte zero.  Each call has an
// independent cursor and reads the exact artifact bytes.
func (a BodyArtifact) Reader() io.Reader { return bytes.NewReader(a.body) }

// Open returns a fresh ReadCloser over the exact artifact bytes.  This mirrors
// a blob store's reader API while keeping this in-memory primitive dependency-
// free.
func (a BodyArtifact) Open() io.ReadCloser { return io.NopCloser(bytes.NewReader(a.body)) }

// Len reports the number of bytes held by this in-memory artifact.
func (a BodyArtifact) Len() int64 { return int64(len(a.body)) }

// Complete reports whether capture reached the end of the application body.
func (a BodyArtifact) Complete() bool { return a.ref.Complete }

// Status reports the capture status of this artifact.
func (a BodyArtifact) Status() CaptureStatus { return a.ref.Status() }

// Verify checks that the in-memory bytes still match the reference's size and
// SHA-256.  It is useful at transport boundaries and in tests.  A false
// result indicates corruption or an incorrectly paired external blob.
func (a BodyArtifact) Verify() bool {
	return a.ref.Size == int64(len(a.body)) && a.ref.SHA256 == SHA256Hex(a.body)
}

// MarshalJSON serializes metadata only.  Body bytes deliberately never appear
// inline in snapshots/events; callers retrieve them by ArtifactID/StorageRef.
func (a BodyArtifact) MarshalJSON() ([]byte, error) { return json.Marshal(a.ref) }

// NewArtifactFromRef pairs an external/reference DTO with bytes retrieved from
// storage.  It copies body and verifies size/hash before returning, preventing
// a stale or wrong blob from becoming transport authority.
func NewArtifactFromRef(ref ArtifactRef, body []byte) (BodyArtifact, error) {
	if ref.Size != int64(len(body)) {
		return BodyArtifact{}, fmt.Errorf("wire: artifact %q size mismatch: ref=%d bytes=%d", ref.ArtifactID, ref.Size, len(body))
	}
	if ref.SHA256 == "" {
		return BodyArtifact{}, fmt.Errorf("wire: artifact %q has empty sha256", ref.ArtifactID)
	}
	if ref.SHA256 != SHA256Hex(body) {
		return BodyArtifact{}, fmt.Errorf("wire: artifact %q sha256 mismatch", ref.ArtifactID)
	}
	return BodyArtifact{ref: ref, body: cloneBytes(body)}, nil
}

// Validate checks metadata invariants without requiring body access.  SHA256
// must be a lowercase/uppercase hexadecimal 32-byte digest when present;
// callers can use Verify after loading bytes for the full integrity check.
func (r ArtifactRef) Validate() error {
	if r.ArtifactID == "" {
		return errors.New("wire: artifact_id is required")
	}
	if r.Size < 0 {
		return errors.New("wire: artifact size cannot be negative")
	}
	if len(r.SHA256) != sha256.Size*2 {
		return errors.New("wire: artifact sha256 must be a 64-character hexadecimal digest")
	}
	if _, err := hex.DecodeString(r.SHA256); err != nil {
		return fmt.Errorf("wire: artifact sha256 is not hexadecimal: %w", err)
	}
	if r.StorageRef == "" {
		return errors.New("wire: storage_ref is required")
	}
	return nil
}

func cloneBytes(body []byte) []byte {
	if body == nil {
		return nil
	}
	copied := make([]byte, len(body))
	copy(copied, body)
	return copied
}

// CaptureOptions controls CaptureReader.  A zero MaxBytes means unlimited.
type CaptureOptions struct {
	ArtifactOptions
	MaxBytes int64
}

// ErrCaptureLimit indicates that CaptureReader stopped at MaxBytes before the
// reader reached EOF.  The returned artifact still contains all bytes that
// were safely captured and is marked incomplete.
var ErrCaptureLimit = errors.New("wire: capture limit exceeded")

// ErrInvalidCaptureLimit indicates a negative MaxBytes value.  Zero is the
// documented unlimited value; negative values are rejected to avoid silently
// disabling a caller's safety limit.
var ErrInvalidCaptureLimit = errors.New("wire: capture limit cannot be negative")

// ErrNoProgress indicates that a reader returned (0, nil) repeatedly.  The
// returned artifact contains bytes captured before the stalled reader.
var ErrNoProgress = errors.New("wire: capture reader made no progress")

const maxNoProgressReads = 100

// CaptureReader reads an application body exactly as supplied by r and
// returns a body artifact.  On a non-EOF read error, context cancellation, or
// capture-limit breach, it returns the partial artifact with Complete=false
// and the underlying error.  The digest on an incomplete artifact is the
// digest of captured bytes, never a claim about bytes that were not observed.
func CaptureReader(r io.Reader, opts CaptureOptions) (BodyArtifact, error) {
	return CaptureReaderContext(context.Background(), r, opts)
}

// CaptureReaderContext is CaptureReader with cancellation checks between read
// operations.  A reader that blocks internally must itself honor ctx; no
// generic io.Reader API can interrupt such a call safely.
func CaptureReaderContext(ctx context.Context, r io.Reader, opts CaptureOptions) (BodyArtifact, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil {
		return NewIncompleteArtifact(nil, opts.ArtifactOptions), errors.New("wire: nil capture reader")
	}
	if opts.MaxBytes < 0 {
		return NewIncompleteArtifact(nil, opts.ArtifactOptions), ErrInvalidCaptureLimit
	}

	var captured bytes.Buffer
	// Avoid a potentially enormous eager allocation when a caller configures
	// a large safety limit; bytes.Buffer grows incrementally as data arrives.
	if opts.MaxBytes > 0 && opts.MaxBytes < 64<<20 {
		captured.Grow(int(opts.MaxBytes))
	}
	chunk := make([]byte, 32*1024)
	noProgress := 0

	for {
		if err := ctx.Err(); err != nil {
			return NewIncompleteArtifact(captured.Bytes(), opts.ArtifactOptions), err
		}

		n, err := r.Read(chunk)
		if n < 0 || n > len(chunk) {
			return NewIncompleteArtifact(captured.Bytes(), opts.ArtifactOptions), fmt.Errorf("wire: invalid capture reader count %d", n)
		}
		if n > 0 {
			noProgress = 0
			if opts.MaxBytes > 0 && int64(captured.Len())+int64(n) > opts.MaxBytes {
				allowed := opts.MaxBytes - int64(captured.Len())
				if allowed > 0 {
					_, _ = captured.Write(chunk[:int(allowed)])
				}
				return NewIncompleteArtifact(captured.Bytes(), opts.ArtifactOptions), ErrCaptureLimit
			}
			_, _ = captured.Write(chunk[:n])
		} else if err == nil {
			noProgress++
			if noProgress >= maxNoProgressReads {
				return NewIncompleteArtifact(captured.Bytes(), opts.ArtifactOptions), ErrNoProgress
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return NewArtifact(captured.Bytes(), opts.ArtifactOptions), nil
			}
			return NewIncompleteArtifact(captured.Bytes(), opts.ArtifactOptions), err
		}
	}
}

// Capture is a concise alias for CaptureReader.
func Capture(r io.Reader, opts CaptureOptions) (BodyArtifact, error) {
	return CaptureReader(r, opts)
}

// CaptureContext is a concise alias for CaptureReaderContext.
func CaptureContext(ctx context.Context, r io.Reader, opts CaptureOptions) (BodyArtifact, error) {
	return CaptureReaderContext(ctx, r, opts)
}

// CaptureTimestamps is a small value used by response envelopes when a body
// is captured while streaming.  It keeps timestamp handling separate from
// body bytes and can be left at zero when unavailable.
type CaptureTimestamps struct {
	StartedAt time.Time
	EndedAt   time.Time
}
