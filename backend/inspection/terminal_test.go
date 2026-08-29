package inspection

import "testing"

func TestIsTerminalStreamRecord(t *testing.T) {
	cases := []struct {
		name     string
		protocol Protocol
		record   SSEEvent
		want     bool
	}{
		{"responses completed by name", ProtocolResponses, SSEEvent{Name: "response.completed"}, true},
		{"responses failed by name", ProtocolResponses, SSEEvent{Name: "response.failed"}, true},
		{"responses incomplete by name", ProtocolResponses, SSEEvent{Name: "response.incomplete"}, true},
		{"responses delta is not terminal", ProtocolResponses, SSEEvent{Name: "response.output_text.delta"}, false},
		{"responses nameless completed by type", ProtocolResponses, SSEEvent{Data: `{"type":"response.completed"}`}, true},
		{"responses nameless failed by type", ProtocolResponses, SSEEvent{Data: `{"type":"response.failed"}`}, true},
		{"responses nameless non-terminal", ProtocolResponses, SSEEvent{Data: `{"type":"response.output_text.delta","delta":"x"}`}, false},
		{"responses nameless malformed data", ProtocolResponses, SSEEvent{Data: `not json`}, false},
		{"responses name with non-terminal type wins", ProtocolResponses, SSEEvent{Name: "response.output_text.delta", Data: `{"type":"response.completed"}`}, false},
		{"chat done sentinel", ProtocolChatCompletions, SSEEvent{Data: "[DONE]"}, true},
		{"chat done sentinel padded", ProtocolChatCompletions, SSEEvent{Data: " [DONE] "}, true},
		{"chat chunk is not terminal", ProtocolChatCompletions, SSEEvent{Data: `{"choices":[]}`}, false},
		{"anthropic message_stop", ProtocolAnthropicMessages, SSEEvent{Name: "message_stop"}, true},
		{"anthropic delta is not terminal", ProtocolAnthropicMessages, SSEEvent{Name: "content_block_delta"}, false},
		{"unknown protocol", ProtocolUnknown, SSEEvent{Name: "response.completed"}, false},
	}
	for _, tc := range cases {
		if got := IsTerminalStreamRecord(tc.protocol, tc.record); got != tc.want {
			t.Errorf("%s: IsTerminalStreamRecord = %v, want %v", tc.name, got, tc.want)
		}
	}
}
