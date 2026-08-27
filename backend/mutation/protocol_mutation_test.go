package mutation

import (
	"bytes"
	"context-lens/backend/inspection"
	"context-lens/backend/wire"
	"os"
	"path/filepath"
	"testing"
)

func mutationFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	path := filepath.Join(append([]string{"..", "..", "tests", "fixtures"}, parts...)...)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return body
}

func TestJSONPatchProtocolPreservesUnknownAndNestedValues(t *testing.T) {
	baseBody := mutationFixture(t, "chat_completions", "json", "response.json")
	base := wire.NewArtifact(baseBody, wire.ArtifactOptions{Stage: wire.StageResponseUpstream, Direction: wire.DirectionUpstream, ContentType: "application/json"})
	before := append([]byte(nil), base.Bytes()...)
	result, err := JSONPatchProtocol(base, []Operation{
		{Op: "replace", Path: "/choices/1/message/content", Value: "edited alternate answer"},
		{Op: "add", Path: "/choices/0/message/provider_extension", Value: map[string]any{"keep": true}},
	}, inspection.ProtocolChatCompletions)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Validated || !result.Validation.Valid || result.Protocol != inspection.ProtocolChatCompletions || result.Format != inspection.FormatJSON {
		t.Fatalf("validation = %#v result=%#v", result.Validation, result)
	}
	if result.Artifact.Ref().Stage != "derived" || result.BaseSHA256 != base.Ref().SHA256 || result.Artifact.Ref().SHA256 == base.Ref().SHA256 {
		t.Fatalf("derived artifact metadata = %#v", result.Artifact.Ref())
	}
	if bytes.Equal(result.Artifact.Bytes(), before) || !bytes.Equal(base.Bytes(), before) {
		t.Fatalf("base artifact changed or candidate did not change")
	}
	paths := make(map[string]bool)
	for _, entry := range result.Diff.Entries {
		paths[entry.Path] = true
	}
	if !paths["/choices/1/message/content"] || !paths["/choices/0/message/provider_extension"] {
		t.Fatalf("field-level diff = %#v", result.Diff)
	}
	projection := inspection.InspectChatCompletionsJSON(result.Artifact.Bytes())
	if projection.Root == nil {
		t.Fatalf("unknown/choices lost after mutation: %#v", projection)
	}
	if _, ok := projection.Root.Lookup("/choices/0/message/provider_extension"); !ok || len(projection.ChoiceItems) != 2 {
		t.Fatalf("unknown/choices lost after mutation: %#v", projection)
	}
}

func TestJSONPatchSupportsArraysEscapesAndRemove(t *testing.T) {
	base := wire.NewArtifact([]byte(`{"a/b":[1,2],"~key":{"x":true}}`), wire.ArtifactOptions{Stage: wire.StageRequestInbound})
	result, err := JSONPatch(base, []Operation{
		{Op: "replace", Path: "/a~1b/1", Value: 3},
		{Op: "add", Path: "/a~1b/-", Value: 4},
		{Op: "remove", Path: "/~0key/x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a/b":[1,3,4],"~key":{}}`
	if string(result.Artifact.Bytes()) != want {
		t.Fatalf("patched body = %s, want %s", result.Artifact.Bytes(), want)
	}
	if len(result.Diff.Entries) != 3 {
		t.Fatalf("diff entries = %#v", result.Diff.Entries)
	}
}

func TestProtocolMutationReturnsCandidateAndValidationForInvalidShape(t *testing.T) {
	base := wire.NewArtifact([]byte(`{"model":"m","messages":[]}`), wire.ArtifactOptions{Stage: wire.StageRequestInbound})
	result, err := JSONPatchProtocol(base, []Operation{{Op: "remove", Path: "/messages"}}, inspection.ProtocolChatCompletions)
	if err != nil {
		t.Fatal(err)
	}
	if result.Validated || result.Validation.Valid || len(result.Validation.Errors) == 0 {
		t.Fatalf("invalid mutation unexpectedly accepted: %#v", result)
	}
	if _, err := RequireValid(result); err == nil {
		t.Fatal("RequireValid accepted invalid candidate")
	}
	if string(base.Bytes()) != `{"model":"m","messages":[]}` {
		t.Fatal("base changed after invalid candidate")
	}
}

func TestProtocolRawReplacementValidatesSSEWithoutRewritingSource(t *testing.T) {
	body := mutationFixture(t, "responses", "sse", "response.sse")
	base := wire.NewArtifact(body, wire.ArtifactOptions{Stage: wire.StageResponseUpstream, Direction: wire.DirectionUpstream, ContentType: "text/event-stream"})
	result, err := ReplaceProtocol(base, body, inspection.ProtocolResponses, inspection.FormatSSE)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Validated || !result.Validation.Valid || !bytes.Equal(result.Artifact.Bytes(), body) || result.Diff.Changed {
		t.Fatalf("unchanged SSE replacement = %#v", result)
	}
	if result.Artifact.Ref().Stage != "derived" || result.Artifact.Ref().SHA256 != base.Ref().SHA256 {
		t.Fatalf("derived same-byte artifact = %#v", result.Artifact.Ref())
	}
}
