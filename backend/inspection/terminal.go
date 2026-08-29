package inspection

import (
	"encoding/json"
	"strings"
)

// IsTerminalStreamRecord reports whether one observed SSE record carries its
// protocol's stream terminator: the record after which no further semantic
// content follows. It answers transport-level completion only — a
// response.failed or response.incomplete record still terminates its stream,
// and the model-level outcome stays visible in the artifact.
//
// Responses names its terminal events in the event field; a nameless record
// is inspected through its JSON type. Chat Completions terminates with the
// [DONE] data sentinel, and Anthropic Messages with message_stop.
func IsTerminalStreamRecord(protocol Protocol, record SSEEvent) bool {
	switch protocol {
	case ProtocolChatCompletions:
		return strings.TrimSpace(record.Data) == "[DONE]"
	case ProtocolAnthropicMessages:
		return record.Name == "message_stop"
	case ProtocolResponses:
		switch record.Name {
		case "response.completed", "response.failed", "response.incomplete":
			return true
		}
		if record.Name != "" {
			return false
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(record.Data), &probe); err != nil {
			return false
		}
		switch probe.Type {
		case "response.completed", "response.failed", "response.incomplete":
			return true
		}
		return false
	default:
		return false
	}
}
