// Package session owns the additive observation projections that group
// captured exchanges into harness sessions: the request summary shown in the
// traffic queue, the upstream-reported context occupancy, and (in later
// phases) the append-only position chain that reconstructs session trees.
//
// Everything here is a projection of captured wire bytes. Nothing in this
// package is ever used as transport input, and no value it computes can
// mutate an artifact.
package session

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"context-lens/backend/inspection"
)

// maxPreviewRunes bounds the queue preview text. The preview is a display
// aid for picking one exchange out of a crowd, not a body substitute.
const maxPreviewRunes = 96

// Summary is the additive capture-time projection carried on an exchange
// snapshot. All fields are optional; zero values are omitted on the wire.
// Model, MessageCount, Preview, and ToolNames are computed from the original
// inbound request artifact. ContextTokens is backfilled from the upstream
// response usage once available and reflects the body actually forwarded.
type Summary struct {
	Model         string   `json:"model,omitempty"`
	MessageCount  int      `json:"message_count,omitempty"`
	Preview       string   `json:"preview,omitempty"`
	ToolNames     []string `json:"tool_names,omitempty"`
	ContextTokens *int64   `json:"context_tokens,omitempty"`
}

// Clone returns an independent copy safe to hand across snapshot copies.
func (s *Summary) Clone() *Summary {
	if s == nil {
		return nil
	}
	c := *s
	if s.ToolNames != nil {
		c.ToolNames = append([]string(nil), s.ToolNames...)
	}
	if s.ContextTokens != nil {
		value := *s.ContextTokens
		c.ContextTokens = &value
	}
	return &c
}

// Empty reports whether the summary carries no observable field.
func (s *Summary) Empty() bool {
	if s == nil {
		return true
	}
	return s.Model == "" && s.MessageCount == 0 && s.Preview == "" && len(s.ToolNames) == 0 && s.ContextTokens == nil
}

// IsChatProtocol reports whether a protocol is one of the three conversation
// protocols the session projections understand.
func IsChatProtocol(protocol inspection.Protocol) bool {
	switch protocol {
	case inspection.ProtocolResponses, inspection.ProtocolChatCompletions, inspection.ProtocolAnthropicMessages:
		return true
	default:
		return false
	}
}

// SummarizeRequest computes the request-side summary fields from the original
// inbound request body bytes. It never fails: an unparseable or non-chat body
// yields an empty Summary the caller may drop.
func SummarizeRequest(protocol inspection.Protocol, body []byte) Summary {
	summary := Summary{}
	if !IsChatProtocol(protocol) {
		return summary
	}
	root := inspection.InspectJSON(body).Root
	if root == nil || root.Kind != inspection.JSONObject {
		return summary
	}
	summary.Model = stringField(root, "model")
	summary.MessageCount = messageSequenceLength(protocol, root)
	summary.ToolNames = toolNames(protocol, root)
	summary.Preview = requestPreview(protocol, root, summary.Model, summary.MessageCount)
	return summary
}

// ExtractContextTokens reads the upstream-reported input-token count from a
// response body. It accepts JSON and SSE bodies and returns nil whenever no
// usage was reported (for example a Chat Completions stream that was sent
// without stream_options.include_usage).
func ExtractContextTokens(protocol inspection.Protocol, body []byte) *int64 {
	if !IsChatProtocol(protocol) {
		return nil
	}
	projection := inspection.InspectProtocol(protocol, body)
	nodes := projection.UsageItems
	if projection.Usage != nil {
		nodes = append([]*inspection.JSONNode{projection.Usage}, nodes...)
	}
	for _, node := range nodes {
		if node == nil || node.Kind != inspection.JSONObject {
			continue
		}
		if tokens := usageInputTokens(protocol, node); tokens != nil {
			return tokens
		}
	}
	return nil
}

// usageInputTokens reads the input-side token total from one usage object.
// Anthropic's input_tokens excludes cache traffic, so the observable context
// occupancy sums the cache fields back in.
func usageInputTokens(protocol inspection.Protocol, usage *inspection.JSONNode) *int64 {
	switch protocol {
	case inspection.ProtocolChatCompletions:
		return numberField(usage, "prompt_tokens")
	case inspection.ProtocolResponses:
		return numberField(usage, "input_tokens")
	case inspection.ProtocolAnthropicMessages:
		total := int64(0)
		found := false
		for _, field := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
			if value := numberField(usage, field); value != nil {
				total += *value
				found = true
			}
		}
		if !found {
			return nil
		}
		return &total
	default:
		return nil
	}
}

// messageSequenceLength counts the normalized message sequence: the
// top-level system/instructions element is a virtual first message, which
// keeps the three protocols comparable (Chat Completions already keeps its
// system prompt inside messages).
func messageSequenceLength(protocol inspection.Protocol, root *inspection.JSONNode) int {
	count := 0
	if system := systemElement(protocol, root); system != nil {
		count++
	}
	switch protocol {
	case inspection.ProtocolChatCompletions, inspection.ProtocolAnthropicMessages:
		if messages := arrayField(root, "messages"); messages != nil {
			count += len(messages.Items)
		}
	case inspection.ProtocolResponses:
		if input := objectField(root, "input"); input != nil {
			if input.Kind == inspection.JSONArray {
				count += len(input.Items)
			} else {
				count++
			}
		}
	}
	return count
}

// systemElement returns the virtual first message for protocols that carry
// the system prompt outside the message array.
func systemElement(protocol inspection.Protocol, root *inspection.JSONNode) *inspection.JSONNode {
	switch protocol {
	case inspection.ProtocolAnthropicMessages:
		return objectField(root, "system")
	case inspection.ProtocolResponses:
		return objectField(root, "instructions")
	default:
		return nil
	}
}

// toolNames collects the names of declared tools. The chat protocols nest the
// name under function, while Responses and Anthropic keep it at the item top
// level.
func toolNames(protocol inspection.Protocol, root *inspection.JSONNode) []string {
	tools := arrayField(root, "tools")
	if tools == nil {
		return nil
	}
	var names []string
	for _, item := range tools.Items {
		if item == nil || item.Kind != inspection.JSONObject {
			continue
		}
		name := stringField(item, "name")
		if name == "" && protocol == inspection.ProtocolChatCompletions {
			if fn := objectField(item, "function"); fn != nil {
				name = stringField(fn, "name")
			}
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// requestPreview derives the queue preview: the first user message text is the
// best human key for "which exchange is this"; any message text follows; the
// model and message count are the final fallback.
func requestPreview(protocol inspection.Protocol, root *inspection.JSONNode, model string, messageCount int) string {
	var firstUser string
	var anyText string
	visitMessages(protocol, root, func(role string, text string) {
		if text == "" {
			return
		}
		if anyText == "" {
			anyText = text
		}
		if role == "user" && firstUser == "" {
			firstUser = text
		}
	})
	if firstUser != "" {
		return truncatePreview(firstUser)
	}
	if anyText != "" {
		return truncatePreview(anyText)
	}
	if model != "" || messageCount > 0 {
		parts := make([]string, 0, 2)
		if model != "" {
			parts = append(parts, model)
		}
		if messageCount > 0 {
			parts = append(parts, strconv.Itoa(messageCount)+" msgs")
		}
		return strings.Join(parts, " · ")
	}
	return ""
}

// visitMessages walks the request's message sequence in order, calling visit
// with each message's role and its first readable text.
func visitMessages(protocol inspection.Protocol, root *inspection.JSONNode, visit func(role string, text string)) {
	if system := systemElement(protocol, root); system != nil {
		if text := nodeText(system); text != "" {
			visit("system", text)
		}
	}
	switch protocol {
	case inspection.ProtocolChatCompletions, inspection.ProtocolAnthropicMessages:
		if messages := arrayField(root, "messages"); messages != nil {
			for _, message := range messages.Items {
				if message == nil || message.Kind != inspection.JSONObject {
					continue
				}
				visit(stringField(message, "role"), messageText(protocol, message))
			}
		}
	case inspection.ProtocolResponses:
		if input := objectField(root, "input"); input != nil {
			if input.Kind == inspection.JSONArray {
				for _, item := range input.Items {
					if item == nil || item.Kind != inspection.JSONObject {
						continue
					}
					visit(stringField(item, "role"), messageText(protocol, item))
				}
			} else if input.Kind == inspection.JSONString {
				visit("user", nodeText(input))
			}
		}
	}
}

// messageText extracts the readable text of one message node: either the
// string form of content or the first text-bearing content block.
func messageText(protocol inspection.Protocol, message *inspection.JSONNode) string {
	if content := objectField(message, "content"); content != nil {
		if text := nodeText(content); text != "" {
			return text
		}
	}
	if content := arrayField(message, "content"); content != nil {
		for _, block := range content.Items {
			if text := blockText(block); text != "" {
				return text
			}
		}
	}
	// Responses items may keep the text directly on the item.
	return blockText(message)
}

// blockText reads the text of a content block such as input_text, output_text,
// text, or thinking.
func blockText(block *inspection.JSONNode) string {
	if block == nil || block.Kind != inspection.JSONObject {
		return ""
	}
	return stringField(block, "text")
}

// nodeText renders a string node or a string/block array as readable text.
func nodeText(node *inspection.JSONNode) string {
	if node == nil {
		return ""
	}
	switch node.Kind {
	case inspection.JSONString:
		text, _ := node.Value.(string)
		return text
	case inspection.JSONArray:
		for _, block := range node.Items {
			if text := blockText(block); text != "" {
				return text
			}
		}
	}
	return ""
}

// truncatePreview collapses whitespace to a single line and bounds the length
// in runes so queue rows stay one line regardless of message size.
func truncatePreview(text string) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	if collapsed == "" {
		return ""
	}
	if utf8.RuneCountInString(collapsed) <= maxPreviewRunes {
		return collapsed
	}
	runes := []rune(collapsed)
	return string(runes[:maxPreviewRunes]) + "…"
}

func objectField(node *inspection.JSONNode, key string) *inspection.JSONNode {
	if node == nil || node.Kind != inspection.JSONObject {
		return nil
	}
	if field, ok := node.Field(key); ok {
		return field.Value
	}
	return nil
}

func arrayField(node *inspection.JSONNode, key string) *inspection.JSONNode {
	value := objectField(node, key)
	if value == nil || value.Kind != inspection.JSONArray {
		return nil
	}
	return value
}

func stringField(node *inspection.JSONNode, key string) string {
	value := objectField(node, key)
	if value == nil || value.Kind != inspection.JSONString {
		return ""
	}
	text, _ := value.Value.(string)
	return strings.TrimSpace(text)
}

func numberField(node *inspection.JSONNode, key string) *int64 {
	value := objectField(node, key)
	if value == nil || value.Kind != inspection.JSONNumber {
		return nil
	}
	raw := strings.TrimSpace(string(value.Raw))
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return &n
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		n := int64(f)
		return &n
	}
	return nil
}
