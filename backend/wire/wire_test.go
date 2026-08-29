package wire

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewArtifactPreservesExactBytesAndMetadata(t *testing.T) {
	body := []byte("{ \"number\":1.2300,\"unknown\":[null,true] }\n\ndata: [DONE]\n\n")
	original := append([]byte(nil), body...)
	artifact := NewArtifact(body, ArtifactOptions{
		ArtifactID:      "artifact-test",
		Stage:           StageRequestInbound,
		Direction:       DirectionInbound,
		ContentType:     "application/json",
		ContentEncoding: "identity",
		StorageRef:      "blob/test",
	})

	if got := artifact.Bytes(); !bytes.Equal(got, original) {
		t.Fatalf("bytes changed: got %q want %q", got, original)
	}
	ref := artifact.Ref()
	if ref.ArtifactID != "artifact-test" || ref.Stage != StageRequestInbound || ref.Direction != DirectionInbound {
		t.Fatalf("metadata = %#v", ref)
	}
	if ref.ContentType != "application/json" || ref.ContentEncoding != "identity" || ref.StorageRef != "blob/test" {
		t.Fatalf("content metadata = %#v", ref)
	}
	if ref.Size != int64(len(original)) || ref.SHA256 != SHA256Hex(original) || !ref.Complete {
		t.Fatalf("integrity metadata = %#v", ref)
	}
	if got := artifact.Status(); got != CaptureComplete {
		t.Fatalf("status = %q, want %q", got, CaptureComplete)
	}
	if !artifact.Verify() {
		t.Fatal("artifact does not verify")
	}

	// Both input and output slices are independent of the authority.
	body[0] = 'X'
	got := artifact.Bytes()
	got[1] = 'Y'
	if !bytes.Equal(artifact.Bytes(), original) {
		t.Fatal("artifact bytes were mutated through caller-owned slice")
	}

	encoded, err := artifact.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal artifact: %v", err)
	}
	if bytes.Contains(encoded, []byte("number")) || !bytes.Contains(encoded, []byte(`"artifact_id":"artifact-test"`)) {
		t.Fatalf("artifact JSON should contain metadata only: %s", encoded)
	}
}

func TestArtifactReaderAndOpenStartAtZero(t *testing.T) {
	artifact := NewArtifact([]byte{0, 1, 2, 0xff}, ArtifactOptions{ArtifactID: "reader-test"})
	first, err := io.ReadAll(artifact.Reader())
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !bytes.Equal(first, []byte{0, 1, 2, 0xff}) {
		t.Fatalf("reader bytes = %v", first)
	}
	secondReader := artifact.Open()
	second, err := io.ReadAll(secondReader)
	closeErr := secondReader.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("open read/close: read=%v close=%v", err, closeErr)
	}
	if !bytes.Equal(second, first) {
		t.Fatalf("open bytes = %v, want %v", second, first)
	}
}

func TestNewArtifactFromRefVerifiesBlob(t *testing.T) {
	body := []byte("opaque application bytes\x00\xff")
	ref := NewArtifactRef(body, ArtifactOptions{ArtifactID: "external-test", StorageRef: "blob-1"})
	artifact, err := NewArtifactFromRef(ref, body)
	if err != nil {
		t.Fatalf("pair ref and body: %v", err)
	}
	if !artifact.Verify() || !bytes.Equal(artifact.Bytes(), body) {
		t.Fatalf("paired artifact invalid: ref=%#v bytes=%q", artifact.Ref(), artifact.Bytes())
	}
	if _, err := NewArtifactFromRef(ref, append([]byte(nil), body[:len(body)-1]...)); err == nil {
		t.Fatal("size mismatch was accepted")
	}
	badHash := ref
	badHash.SHA256 = SHA256Hex([]byte("other"))
	if _, err := NewArtifactFromRef(badHash, body); err == nil {
		t.Fatal("hash mismatch was accepted")
	}
}

func TestCaptureReaderLargeBody(t *testing.T) {
	const size = 8 << 20
	body := make([]byte, size)
	for i := range body {
		body[i] = byte((i*31 + 17) % 251)
	}
	artifact, err := CaptureReader(bytes.NewReader(body), CaptureOptions{
		ArtifactOptions: ArtifactOptions{ArtifactID: "large-capture", Stage: StageResponseUpstream},
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !artifact.Complete() || artifact.Len() != size {
		t.Fatalf("capture status/size: complete=%v size=%d", artifact.Complete(), artifact.Len())
	}
	if !bytes.Equal(artifact.Bytes(), body) {
		t.Fatal("large body changed during capture")
	}
	if artifact.Ref().SHA256 != SHA256Hex(body) {
		t.Fatalf("hash = %s, want %s", artifact.Ref().SHA256, SHA256Hex(body))
	}
}

type partialReader struct {
	body []byte
	err  error
	done bool
}

func (r *partialReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	n := copy(p, r.body)
	r.done = true
	return n, r.err
}

func TestCaptureReaderIncompletePreservesCapturedPrefix(t *testing.T) {
	want := []byte("prefix\x00with\xffbytes")
	readErr := errors.New("upstream body interrupted")
	artifact, err := CaptureReader(&partialReader{body: want, err: readErr}, CaptureOptions{
		ArtifactOptions: ArtifactOptions{ArtifactID: "partial-capture", Stage: StageResponseUpstream},
	})
	if !errors.Is(err, readErr) {
		t.Fatalf("capture error = %v, want %v", err, readErr)
	}
	if artifact.Complete() || artifact.Status() != CaptureIncomplete {
		t.Fatalf("incomplete status = complete:%v status:%q", artifact.Complete(), artifact.Status())
	}
	if !bytes.Equal(artifact.Bytes(), want) {
		t.Fatalf("captured bytes = %q, want %q", artifact.Bytes(), want)
	}
	if artifact.Ref().Size != int64(len(want)) || artifact.Ref().SHA256 != SHA256Hex(want) {
		t.Fatalf("partial integrity = %#v", artifact.Ref())
	}
}

func TestCaptureReaderLimitMarksIncomplete(t *testing.T) {
	artifact, err := CaptureReader(strings.NewReader("0123456789"), CaptureOptions{
		ArtifactOptions: ArtifactOptions{ArtifactID: "limited-capture"},
		MaxBytes:        4,
	})
	if !errors.Is(err, ErrCaptureLimit) {
		t.Fatalf("capture error = %v, want ErrCaptureLimit", err)
	}
	if artifact.Complete() || string(artifact.Bytes()) != "0123" {
		t.Fatalf("limited capture = complete:%v body:%q", artifact.Complete(), artifact.Bytes())
	}
	_, err = CaptureReader(strings.NewReader("ignored"), CaptureOptions{
		ArtifactOptions: ArtifactOptions{ArtifactID: "invalid-limit"},
		MaxBytes:        -1,
	})
	if !errors.Is(err, ErrInvalidCaptureLimit) {
		t.Fatalf("negative limit error = %v, want ErrInvalidCaptureLimit", err)
	}
}

func TestCaptureReaderContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	artifact, err := CaptureReaderContext(ctx, strings.NewReader("never read"), CaptureOptions{ArtifactOptions: ArtifactOptions{ArtifactID: "cancelled-capture"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("capture error = %v, want context.Canceled", err)
	}
	if artifact.Complete() || len(artifact.Bytes()) != 0 {
		t.Fatalf("cancelled capture = complete:%v body:%q", artifact.Complete(), artifact.Bytes())
	}
}

func TestArtifactRefValidate(t *testing.T) {
	ref := NewArtifactRef([]byte("ok"), ArtifactOptions{ArtifactID: "valid", StorageRef: "store"})
	if err := ref.Validate(); err != nil {
		t.Fatalf("valid ref rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ArtifactRef){
		"id":      func(r *ArtifactRef) { r.ArtifactID = "" },
		"size":    func(r *ArtifactRef) { r.Size = -1 },
		"hash":    func(r *ArtifactRef) { r.SHA256 = "bad" },
		"storage": func(r *ArtifactRef) { r.StorageRef = "" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := ref
			mutate(&copy)
			if err := copy.Validate(); err == nil {
				t.Fatal("invalid ref accepted")
			}
		})
	}
}

func TestRequestEnvelopePreservesEscapedPathAndRawQuery(t *testing.T) {
	reqURL := &url.URL{Scheme: "http", Host: "example.test", Path: "/v1/a/b", RawPath: "/v1/a%2Fb", RawQuery: "x=%2F&x=1+2&z=%26"}
	req := &http.Request{
		Method: "POST",
		URL:    reqURL,
		Header: http.Header{"X-Test": {"one", "two"}, "Authorization": {"Bearer secret"}},
	}
	envelope := RequestFromHTTP(req)
	if envelope.Method != "POST" || envelope.Path != "/v1/a/b" || envelope.EscapedPath != "/v1/a%2Fb" || envelope.RawQuery != reqURL.RawQuery {
		t.Fatalf("envelope = %#v", envelope)
	}
	req.Header["X-Test"][0] = "changed"
	if envelope.Headers.Get("X-Test") != "one" {
		t.Fatal("request headers were not copied")
	}
	redacted := envelope.Redacted()
	if got := redacted.Headers.Get("Authorization"); got != RedactedHeaderValue {
		t.Fatalf("authorization = %q", got)
	}
	if got := redacted.Headers.Get("X-Test"); got != "one" {
		t.Fatalf("non-sensitive header = %q", got)
	}
	if envelope.Headers.Get("Authorization") != "Bearer secret" {
		t.Fatal("redaction mutated original envelope")
	}
}

func TestResponseEnvelopeCopiesAndRedactsHeadersAndTrailers(t *testing.T) {
	started := time.Unix(10, 20).UTC()
	ended := time.Unix(11, 21).UTC()
	headers := http.Header{"Content-Type": {"application/json"}, "X-Api-Key": {"secret"}}
	trailers := http.Header{"Set-Cookie": {"session=secret"}, "X-End": {"done"}}
	envelope := NewResponseEnvelope(http.StatusBadGateway, headers, trailers, started, ended)
	headers.Set("Content-Type", "changed")
	trailers.Set("X-End", "changed")
	if envelope.Status != http.StatusBadGateway || !envelope.StartedAt.Equal(started) || !envelope.EndedAt.Equal(ended) {
		t.Fatalf("metadata = %#v", envelope)
	}
	if envelope.Headers.Get("Content-Type") != "application/json" || envelope.Trailers.Get("X-End") != "done" {
		t.Fatalf("headers were not copied: %#v %#v", envelope.Headers, envelope.Trailers)
	}
	redacted := envelope.Redacted()
	if redacted.Headers.Get("X-Api-Key") != RedactedHeaderValue || redacted.Trailers.Get("Set-Cookie") != RedactedHeaderValue {
		t.Fatalf("sensitive values not redacted: %#v %#v", redacted.Headers, redacted.Trailers)
	}
}

func TestHeaderRedactionAndValidation(t *testing.T) {
	input := http.Header{
		"authorization":      {"Bearer secret"},
		"X-API-KEY":          {"one", "two"},
		"X-Context-Lens-Key": {"client-secret"},
		"X-Trace":            {"a", "b"},
	}
	redacted := RedactHeaders(input)
	if len(redacted.Values("X-API-KEY")) != 1 || redacted.Get("X-API-KEY") != RedactedHeaderValue {
		t.Fatalf("multi-value credential = %#v", redacted.Values("X-API-KEY"))
	}
	if !strings.EqualFold(redacted.Get("Authorization"), RedactedHeaderValue) {
		t.Fatalf("authorization = %#v", redacted.Values("Authorization"))
	}
	if redacted.Get("X-Context-Lens-Key") != RedactedHeaderValue {
		t.Fatalf("context-lens key was not redacted")
	}
	if strings.Join(redacted["X-Trace"], ",") != "a,b" {
		t.Fatalf("trace changed = %#v", redacted["X-Trace"])
	}
	input["X-Trace"][0] = "mutated"
	if redacted["X-Trace"][0] != "a" {
		t.Fatal("redaction did not deep-copy values")
	}
	if err := ValidateHeaders(http.Header{"X-OK": {"value", ""}}); err != nil {
		t.Fatalf("valid headers rejected: %v", err)
	}
	for _, bad := range []http.Header{
		{"X-Bad\nName": {"value"}},
		{"X-Bad": {"value\r\nnext"}},
	} {
		if err := ValidateHeaders(bad); err == nil {
			t.Fatalf("invalid header accepted: %#v", bad)
		}
	}
}

func TestSHA256HexMatchesStandardLibrary(t *testing.T) {
	body := []byte("hash me")
	digest := sha256.Sum256(body)
	if got, want := SHA256Hex(body), hex.EncodeToString(digest[:]); got != want {
		t.Fatalf("digest = %s, want %s", got, want)
	}
}
