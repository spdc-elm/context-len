package inspection

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	path := filepath.Join(append([]string{"..", "..", "tests", "fixtures"}, parts...)...)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return body
}

func TestProtocolJSONFixturesRetainSectionsAndUnknownFields(t *testing.T) {
	cases := []struct {
		name     string
		protocol Protocol
		path     []string
		kind     PayloadKind
		unknown  string
		check    func(*testing.T, ProtocolProjection)
	}{
		{"responses request", ProtocolResponses, []string{"responses", "json", "request.json"}, PayloadRequest, "/provider_extension", func(t *testing.T, p ProtocolProjection) {
			if p.Model == nil || p.Input == nil || p.Tools == nil || len(p.ContentBlocks) != 2 {
				t.Fatalf("responses request sections: model=%v input=%v tools=%v blocks=%d", p.Model != nil, p.Input != nil, p.Tools != nil, len(p.ContentBlocks))
			}
		}},
		{"responses response", ProtocolResponses, []string{"responses", "json", "response.json"}, PayloadResponse, "/unknown_response_field", func(t *testing.T, p ProtocolProjection) {
			if len(p.OutputItems) != 3 || len(p.ReasoningItems) != 1 || p.Usage == nil {
				t.Fatalf("responses output=%d reasoning=%d usage=%v", len(p.OutputItems), len(p.ReasoningItems), p.Usage != nil)
			}
		}},
		{"chat request", ProtocolChatCompletions, []string{"chat_completions", "json", "request.json"}, PayloadRequest, "/unknown_provider_field", func(t *testing.T, p ProtocolProjection) {
			if p.Messages == nil || p.Tools == nil {
				t.Fatalf("chat request sections: messages=%v tools=%v", p.Messages != nil, p.Tools != nil)
			}
		}},
		{"chat response", ProtocolChatCompletions, []string{"chat_completions", "json", "response.json"}, PayloadResponse, "/unknown_response_field", func(t *testing.T, p ProtocolProjection) {
			if len(p.ChoiceItems) != 2 || p.Usage == nil || p.ChoiceItems[1].Index == nil || *p.ChoiceItems[1].Index != 1 {
				t.Fatalf("chat choices=%d usage=%v second=%#v", len(p.ChoiceItems), p.Usage != nil, p.ChoiceItems)
			}
		}},
		{"anthropic request", ProtocolAnthropicMessages, []string{"anthropic_messages", "json", "request.json"}, PayloadRequest, "/unknown_provider_field", func(t *testing.T, p ProtocolProjection) {
			if p.Messages == nil || len(p.ThinkingBlocks) != 1 || len(p.SignatureNodes) != 1 || len(p.ToolUseBlocks) != 1 || len(p.ToolResultBlocks) != 1 {
				t.Fatalf("anthropic request sections: messages=%v thinking=%d signatures=%d tools=%d results=%d", p.Messages != nil, len(p.ThinkingBlocks), len(p.SignatureNodes), len(p.ToolUseBlocks), len(p.ToolResultBlocks))
			}
		}},
		{"anthropic response", ProtocolAnthropicMessages, []string{"anthropic_messages", "json", "response.json"}, PayloadResponse, "/unknown_response_field", func(t *testing.T, p ProtocolProjection) {
			if len(p.ThinkingBlocks) != 1 || len(p.RedactedThinking) != 1 || len(p.SignatureNodes) != 1 || len(p.ToolUseBlocks) != 1 {
				t.Fatalf("anthropic response sections: thinking=%d redacted=%d signatures=%d tools=%d", len(p.ThinkingBlocks), len(p.RedactedThinking), len(p.SignatureNodes), len(p.ToolUseBlocks))
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fixture(t, tc.path...)
			before := append([]byte(nil), body...)
			p := InspectProtocol(tc.protocol, body, FormatJSON)
			if p.Kind != tc.kind || !p.Valid || !p.Validation.Valid || p.Status != ParseOK {
				t.Fatalf("projection kind=%s valid=%v validation=%v status=%s errors=%v", p.Kind, p.Valid, p.Validation.Valid, p.Status, p.Validation.ErrorMessages())
			}
			if !bytes.Equal(body, before) || !bytes.Equal(p.Source, body) || p.SourceHash != SourceHash(body) {
				t.Fatalf("protocol inspection changed source bytes")
			}
			found := false
			for _, unknown := range p.UnknownNodes {
				if unknown.Pointer == tc.unknown {
					found = true
					unknown.Raw[0] = 'X'
					break
				}
			}
			if !found {
				t.Fatalf("unknown pointer %q not retained: %#v", tc.unknown, p.UnknownNodes)
			}
			if !bytes.Equal(body, before) {
				t.Fatalf("unknown projection aliases source")
			}
			tc.check(t, p)
		})
	}
}

func TestProtocolSSEFixturesPreserveGrammarAndTermination(t *testing.T) {
	responsesBody := fixture(t, "responses", "sse", "response.sse")
	responses := InspectResponsesSSE(responsesBody)
	if !responses.Valid || !responses.Validation.Valid || responses.Kind != PayloadStream || len(responses.Events) != 14 || len(responses.SequenceNumbers) != 14 || !responses.ProtocolComplete {
		t.Fatalf("responses SSE projection: valid=%v validation=%v kind=%s events=%d seq=%d complete=%v errors=%v", responses.Valid, responses.Validation.Valid, responses.Kind, len(responses.Events), len(responses.SequenceNumbers), responses.ProtocolComplete, responses.Validation.ErrorMessages())
	}
	for i, number := range responses.SequenceNumbers {
		if int(number) != i {
			t.Fatalf("responses sequence[%d]=%d", i, number)
		}
	}
	chatBody := fixture(t, "chat_completions", "sse", "response.sse")
	chat := InspectChatCompletionsSSE(chatBody)
	if !chat.Valid || !chat.Validation.Valid || len(chat.Events) != 7 || !chat.SawDoneSentinel || !chat.ProtocolComplete {
		t.Fatalf("chat SSE projection: valid=%v validation=%v events=%d done=%v complete=%v errors=%v", chat.Valid, chat.Validation.Valid, len(chat.Events), chat.SawDoneSentinel, chat.ProtocolComplete, chat.Validation.ErrorMessages())
	}
	if len(chat.UnknownNodes) != 2 || len(chat.Events[2].UnknownNodes) != 1 || len(chat.Events[5].UnknownNodes) != 1 {
		t.Fatalf("chat SSE unknown nodes: total=%d event2=%d event5=%d", len(chat.UnknownNodes), len(chat.Events[2].UnknownNodes), len(chat.Events[5].UnknownNodes))
	}
	anthropicBody := fixture(t, "anthropic_messages", "sse", "response.sse")
	anthropic := InspectAnthropicMessagesSSE(anthropicBody)
	if !anthropic.Valid || !anthropic.Validation.Valid || len(anthropic.Events) != 14 || len(anthropic.ContentBlockIndices) != 10 || !anthropic.ProtocolComplete {
		t.Fatalf("anthropic SSE projection: valid=%v validation=%v events=%d indexes=%d complete=%v errors=%v", anthropic.Valid, anthropic.Validation.Valid, len(anthropic.Events), len(anthropic.ContentBlockIndices), anthropic.ProtocolComplete, anthropic.Validation.ErrorMessages())
	}
	if len(anthropic.ThinkingBlocks) == 0 || len(anthropic.RedactedThinking) == 0 || len(anthropic.SignatureNodes) == 0 {
		t.Fatalf("anthropic SSE thinking/redacted/signature fields not retained")
	}
	if bytes.Equal(anthropic.Source, nil) {
		t.Fatalf("anthropic SSE source unexpectedly empty")
	}
}

func TestResponsesDoNotTreatDoneAsCompletion(t *testing.T) {
	body := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":0,\"delta\":\"x\"}\n\ndata: [DONE]\n\n")
	p := InspectResponsesSSE(body)
	if p.ProtocolComplete || p.Validation.Valid || len(p.Validation.Errors) == 0 {
		t.Fatalf("Responses [DONE] unexpectedly accepted as completion: complete=%v validation=%#v", p.ProtocolComplete, p.Validation)
	}
	if len(p.Events) != 2 || !p.Events[1].DoneSentinel {
		t.Fatalf("Responses sentinel was not retained as an event: %#v", p.Events)
	}
}
