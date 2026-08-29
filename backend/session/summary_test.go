package session

import (
	"os"
	"path/filepath"
	"testing"

	"context-lens/backend/inspection"
)

func fixtureBody(t *testing.T, parts ...string) []byte {
	t.Helper()
	path := filepath.Join(append([]string{"..", "..", "tests", "fixtures"}, parts...)...)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return body
}

// TestSummarizeRequestFixtureProtocols covers the three conversation
// protocols with the shared wire fixtures: model, normalized message count
// (virtual system element included), tool names, and first-user preview.
func TestSummarizeRequestFixtureProtocols(t *testing.T) {
	cases := []struct {
		protocol     inspection.Protocol
		bodyPath     string
		model        string
		messageCount int
		toolNames    []string
		preview      string
	}{
		{
			protocol:     inspection.ProtocolChatCompletions,
			bodyPath:     "json/request.json",
			model:        "fixture-chat-model",
			messageCount: 4,
			toolNames:    []string{"lookup_fixture"},
			preview:      "Call the lookup tool, then answer.",
		},
		{
			protocol:     inspection.ProtocolAnthropicMessages,
			bodyPath:     "json/request.json",
			model:        "fixture-anthropic-model",
			messageCount: 4, // virtual system + 3 messages
			toolNames:    []string{"lookup_fixture"},
			preview:      "Use the lookup tool and show a compact answer.",
		},
		{
			protocol:     inspection.ProtocolResponses,
			bodyPath:     "json/request.json",
			model:        "fixture-responses-model",
			messageCount: 3, // virtual instructions + 2 input items
			toolNames:    []string{"lookup_fixture"},
			preview:      "Summarize this fixture-safe payload.",
		},
	}
	for _, tc := range cases {
		t.Run(string(tc.protocol), func(t *testing.T) {
			body := fixtureBody(t, string(tc.protocol), tc.bodyPath)
			summary := SummarizeRequest(tc.protocol, body)
			if summary.Model != tc.model {
				t.Fatalf("model = %q, want %q", summary.Model, tc.model)
			}
			if summary.MessageCount != tc.messageCount {
				t.Fatalf("message count = %d, want %d", summary.MessageCount, tc.messageCount)
			}
			if len(summary.ToolNames) != len(tc.toolNames) || summary.ToolNames[0] != tc.toolNames[0] {
				t.Fatalf("tool names = %v, want %v", summary.ToolNames, tc.toolNames)
			}
			if summary.Preview != tc.preview {
				t.Fatalf("preview = %q, want %q", summary.Preview, tc.preview)
			}
			if summary.ContextTokens != nil {
				t.Fatalf("request summary must not carry context tokens")
			}
		})
	}
}

func TestSummarizeRequestFallbacks(t *testing.T) {
	// No user message: preview falls back to any message text.
	systemOnly := []byte(`{"model":"m","messages":[{"role":"system","content":"system text"}]}`)
	summary := SummarizeRequest(inspection.ProtocolChatCompletions, systemOnly)
	if summary.Preview != "system text" {
		t.Fatalf("fallback preview = %q, want system text", summary.Preview)
	}
	if summary.MessageCount != 1 {
		t.Fatalf("message count = %d, want 1", summary.MessageCount)
	}

	// No text at all: preview falls back to model and count.
	noText := []byte(`{"model":"m","messages":[{"role":"user","content":null}]}`)
	summary = SummarizeRequest(inspection.ProtocolChatCompletions, noText)
	if summary.Preview != "m · 1 msgs" {
		t.Fatalf("model fallback preview = %q", summary.Preview)
	}

	// Whitespace is collapsed and long previews are truncated on rune boundaries.
	long := ""
	for i := 0; i < 40; i++ {
		long += "字word "
	}
	longBody := []byte(`{"model":"m","messages":[{"role":"user","content":"` + long + `"}]}`)
	summary = SummarizeRequest(inspection.ProtocolChatCompletions, longBody)
	if got := len([]rune(summary.Preview)); got != maxPreviewRunes+1 {
		t.Fatalf("truncated preview rune length = %d, want %d", got, maxPreviewRunes+1)
	}

	// Unknown protocols and unparseable bodies yield an empty summary.
	if s := SummarizeRequest(inspection.ProtocolUnknown, []byte(`{"model":"m"}`)); !s.Empty() {
		t.Fatalf("unknown protocol summary = %+v, want empty", s)
	}
	if s := SummarizeRequest(inspection.ProtocolChatCompletions, []byte("not json")); !s.Empty() {
		t.Fatalf("malformed summary = %+v, want empty", s)
	}

	// A Responses request whose input is the shorthand string form.
	shorthand := []byte(`{"model":"m","instructions":"be brief","input":"plain user turn"}`)
	summary = SummarizeRequest(inspection.ProtocolResponses, shorthand)
	if summary.MessageCount != 2 {
		t.Fatalf("shorthand message count = %d, want 2", summary.MessageCount)
	}
	if summary.Preview != "plain user turn" {
		t.Fatalf("shorthand preview = %q", summary.Preview)
	}
}

// TestExtractContextTokensJSON covers non-streaming responses: the token
// total is the upstream-reported input side, with Anthropic cache fields
// summed back into the observable occupancy.
func TestExtractContextTokensJSON(t *testing.T) {
	cases := []struct {
		protocol inspection.Protocol
		body     []byte
		want     int64
	}{
		{inspection.ProtocolChatCompletions, fixtureBody(t, "chat_completions", "json", "response.json"), 31},
		{inspection.ProtocolResponses, fixtureBody(t, "responses", "json", "response.json"), 42},
		{inspection.ProtocolAnthropicMessages, fixtureBody(t, "anthropic_messages", "json", "response.json"), 37},
		{
			inspection.ProtocolAnthropicMessages,
			[]byte(`{"usage":{"input_tokens":20,"cache_read_input_tokens":50000,"cache_creation_input_tokens":300,"output_tokens":7}}`),
			50320,
		},
	}
	for _, tc := range cases {
		got := ExtractContextTokens(tc.protocol, tc.body)
		if got == nil || *got != tc.want {
			t.Fatalf("%s JSON tokens = %v, want %d", tc.protocol, got, tc.want)
		}
	}
}

// TestExtractContextTokensSSE covers streaming responses. Usage arrives in a
// terminal or opening event depending on the protocol; nodes without input
// tokens (such as Anthropic message_delta output-only usage) are skipped.
func TestExtractContextTokensSSE(t *testing.T) {
	cases := []struct {
		protocol inspection.Protocol
		body     []byte
		want     int64
	}{
		{inspection.ProtocolChatCompletions, fixtureBody(t, "chat_completions", "sse", "response.sse"), 18},
		{inspection.ProtocolResponses, fixtureBody(t, "responses", "sse", "response.sse"), 21},
		{inspection.ProtocolAnthropicMessages, fixtureBody(t, "anthropic_messages", "sse", "response.sse"), 19},
		{
			inspection.ProtocolAnthropicMessages,
			[]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":9}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
			12,
		},
	}
	for _, tc := range cases {
		got := ExtractContextTokens(tc.protocol, tc.body)
		if got == nil || *got != tc.want {
			t.Fatalf("%s SSE tokens = %v, want %d", tc.protocol, got, tc.want)
		}
	}
}

// TestExtractContextTokensMissing documents the deliberate gap: streams sent
// without include_usage report nothing, and the summary must show that
// absence rather than an estimate.
func TestExtractContextTokensMissing(t *testing.T) {
	noUsage := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	if got := ExtractContextTokens(inspection.ProtocolChatCompletions, noUsage); got != nil {
		t.Fatalf("tokens = %v, want nil", got)
	}
	if got := ExtractContextTokens(inspection.ProtocolChatCompletions, nil); got != nil {
		t.Fatalf("empty body tokens = %v, want nil", got)
	}
	if got := ExtractContextTokens(inspection.ProtocolUnknown, []byte(`{"usage":{"input_tokens":5}}`)); got != nil {
		t.Fatalf("unknown protocol tokens = %v, want nil", got)
	}
}

// TestSummaryClone guards the snapshot-copy boundary: a cloned summary never
// shares slices or pointers with the original.
func TestSummaryClone(t *testing.T) {
	tokens := int64(5)
	summary := &Summary{Model: "m", MessageCount: 2, Preview: "p", ToolNames: []string{"a"}, ContextTokens: &tokens}
	clone := summary.Clone()
	clone.ToolNames[0] = "b"
	*clone.ContextTokens = 99
	if summary.ToolNames[0] != "a" || *summary.ContextTokens != 5 {
		t.Fatalf("clone shares storage with original")
	}
	if summary.Clone() == nil || (*Summary)(nil).Clone() != nil {
		t.Fatalf("clone of nil summary must stay nil")
	}
}
