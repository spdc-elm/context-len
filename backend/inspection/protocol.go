package inspection

import (
	"encoding/json"
	"strconv"
	"strings"
)

// PayloadKind identifies the broad wire shape of a protocol body.  It is an
// observation only: request and response projections never become transport
// input.
type PayloadKind string

const (
	PayloadUnknown  PayloadKind = "unknown"
	PayloadRequest  PayloadKind = "request"
	PayloadResponse PayloadKind = "response"
	PayloadEvent    PayloadKind = "event"
	PayloadStream   PayloadKind = "stream"
)

// ValidationIssue is a protocol validation diagnostic.  Validation is kept
// separate from parser warnings because an unknown/provider extension is
// valid to forward unchanged, while a missing required field may make an
// explicit mutation unsafe to release.
type ValidationIssue struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Pointer string   `json:"pointer,omitempty"`
	Span    ByteSpan `json:"span"`
	Fatal   bool     `json:"fatal,omitempty"`
}

// ValidationResult reports protocol-aware validation of a body.  It never
// changes the input bytes.  Errors are shape/type violations; warnings retain
// useful diagnostics for incomplete streams and provider extensions.
type ValidationResult struct {
	Protocol Protocol          `json:"protocol"`
	Format   BodyFormat        `json:"format"`
	Kind     PayloadKind       `json:"kind"`
	Valid    bool              `json:"valid"`
	Errors   []ValidationIssue `json:"errors,omitempty"`
	Warnings []ValidationIssue `json:"warnings,omitempty"`
}

func (v ValidationResult) ErrorMessages() []string {
	if len(v.Errors) == 0 {
		return nil
	}
	out := make([]string, len(v.Errors))
	for i, item := range v.Errors {
		out[i] = item.Message
	}
	return out
}

func (v ValidationResult) WarningMessages() []string {
	if len(v.Warnings) == 0 {
		return nil
	}
	out := make([]string, len(v.Warnings))
	for i, item := range v.Warnings {
		out[i] = item.Message
	}
	return out
}

// ProtocolEvent is a loss-aware semantic view of one SSE record.  Raw and
// Data are copied from SSEInspection and remain the only bytes that could be
// used for an unchanged release.  Payload points into the event JSON
// projection, never into a caller-owned slice.
type ProtocolEvent struct {
	Ordinal           int             `json:"ordinal"`
	Name              string          `json:"name,omitempty"`
	Type              string          `json:"type,omitempty"`
	Data              string          `json:"data,omitempty"`
	DoneSentinel      bool            `json:"done_sentinel,omitempty"`
	SequenceNumber    *int64          `json:"sequence_number,omitempty"`
	ContentBlockIndex *int64          `json:"content_block_index,omitempty"`
	Payload           *JSONNode       `json:"payload,omitempty"`
	JSON              *JSONInspection `json:"json,omitempty"`
	UnknownNodes      []UnknownNode   `json:"unknown_nodes,omitempty"`
	Warnings          []Warning       `json:"warnings,omitempty"`
	Raw               []byte          `json:"raw"`
	Span              ByteSpan        `json:"span"`
	Complete          bool            `json:"complete"`
}

// ProtocolChoice retains a complete Chat Completions choice.  Message and
// Delta are pointers into the loss-aware JSON tree; all choices, including
// choices with a non-zero index, are retained.
type ProtocolChoice struct {
	Ordinal      int       `json:"ordinal"`
	Index        *int64    `json:"index,omitempty"`
	Node         *JSONNode `json:"node,omitempty"`
	Message      *JSONNode `json:"message,omitempty"`
	Delta        *JSONNode `json:"delta,omitempty"`
	FinishReason *JSONNode `json:"finish_reason,omitempty"`
	Logprobs     *JSONNode `json:"logprobs,omitempty"`
}

// ProtocolProjection is the common projection returned by all three
// protocol-specific inspectors.  The generic JSON/SSE trees are retained in
// full, while the convenience fields identify protocol sections without
// throwing away unknown fields or raw spans.
type ProtocolProjection struct {
	Protocol         Protocol    `json:"protocol"`
	Format           BodyFormat  `json:"format"`
	Kind             PayloadKind `json:"kind"`
	Status           ParseStatus `json:"status"`
	Valid            bool        `json:"valid"`
	Complete         bool        `json:"complete"`
	ProtocolComplete bool        `json:"protocol_complete"`
	Source           []byte      `json:"-"`
	SourceHash       string      `json:"source_hash"`

	JSON *JSONInspection `json:"json,omitempty"`
	SSE  *SSEInspection  `json:"sse,omitempty"`
	Root *JSONNode       `json:"root,omitempty"`

	// Common request/response sections.  Values remain JSONNodes so nested
	// content blocks, provider extensions, duplicate keys, and exact raw
	// spelling stay observable.
	Model        *JSONNode `json:"model,omitempty"`
	Input        *JSONNode `json:"input,omitempty"`
	Instructions *JSONNode `json:"instructions,omitempty"`
	Messages     *JSONNode `json:"messages,omitempty"`
	System       *JSONNode `json:"system,omitempty"`
	Tools        *JSONNode `json:"tools,omitempty"`
	ToolChoice   *JSONNode `json:"tool_choice,omitempty"`
	Output       *JSONNode `json:"output,omitempty"`
	Choices      *JSONNode `json:"choices,omitempty"`
	Usage        *JSONNode `json:"usage,omitempty"`

	OutputItems      []*JSONNode `json:"output_items,omitempty"`
	ReasoningItems   []*JSONNode `json:"reasoning_items,omitempty"`
	ToolCallItems    []*JSONNode `json:"tool_call_items,omitempty"`
	ContentBlocks    []*JSONNode `json:"content_blocks,omitempty"`
	ThinkingBlocks   []*JSONNode `json:"thinking_blocks,omitempty"`
	RedactedThinking []*JSONNode `json:"redacted_thinking,omitempty"`
	SignatureNodes   []*JSONNode `json:"signature_nodes,omitempty"`
	ToolUseBlocks    []*JSONNode `json:"tool_use_blocks,omitempty"`
	ToolResultBlocks []*JSONNode `json:"tool_result_blocks,omitempty"`

	ChoiceItems         []ProtocolChoice `json:"choice_items,omitempty"`
	UsageItems          []*JSONNode      `json:"usage_items,omitempty"`
	Events              []ProtocolEvent  `json:"events,omitempty"`
	SequenceNumbers     []int64          `json:"sequence_numbers,omitempty"`
	ContentBlockIndices []int64          `json:"content_block_indices,omitempty"`
	SawDoneSentinel     bool             `json:"saw_done_sentinel,omitempty"`

	UnknownNodes []UnknownNode    `json:"unknown_nodes,omitempty"`
	Warnings     []Warning        `json:"warnings,omitempty"`
	Validation   ValidationResult `json:"validation"`
}

// ResponsesProjection, ChatCompletionsProjection, and
// AnthropicMessagesProjection are named aliases for callers that want a
// protocol-specific type while sharing one loss-aware DTO shape.
type ResponsesProjection = ProtocolProjection
type ChatCompletionsProjection = ProtocolProjection
type AnthropicMessagesProjection = ProtocolProjection

// ProtocolValidation is a readable alias used by mutation callers.
type ProtocolValidation = ValidationResult

// InspectProtocol parses a protocol body as JSON or SSE and builds a
// protocol-aware projection.  If format is omitted (or FormatUnknown), it is
// inferred conservatively from the body.  A parser/validation warning never
// blocks bypass forwarding.
func InspectProtocol(protocol Protocol, source []byte, format ...BodyFormat) ProtocolProjection {
	chosen := FormatUnknown
	if len(format) > 0 {
		chosen = format[0]
	}
	if chosen == FormatUnknown {
		chosen = inferBodyFormat(source)
	}
	projection := ProtocolProjection{
		Protocol:   protocol,
		Format:     chosen,
		Kind:       PayloadUnknown,
		Status:     ParseOK,
		Valid:      true,
		Complete:   true,
		Source:     cloneBytes(source),
		SourceHash: SourceHash(source),
	}
	switch chosen {
	case FormatJSON:
		jsonProjection := InspectJSONWithSchema(source, protocolSchema(protocol))
		projection.JSON = &jsonProjection
		projection.Root = jsonProjection.Root
		projection.Status = jsonProjection.Status
		projection.Valid = jsonProjection.Valid
		projection.Complete = jsonProjection.Complete
		projection.UnknownNodes = cloneUnknownNodes(jsonProjection.UnknownNodes)
		projection.Warnings = cloneWarnings(jsonProjection.Warnings)
		populateJSONSections(&projection)
	case FormatSSE:
		sseProjection := InspectSSE(source)
		projection.SSE = &sseProjection
		projection.Status = sseProjection.Status
		projection.Valid = sseProjection.Valid
		projection.Complete = sseProjection.Complete
		projection.UnknownNodes = cloneUnknownNodes(sseProjection.UnknownNodes)
		projection.Warnings = cloneWarnings(sseProjection.Warnings)
		populateSSESections(&projection, protocol)
	default:
		projection.Valid = false
		projection.Status = ParseInvalid
		projection.Warnings = append(projection.Warnings, warning("unknown_body_format", "body is neither JSON nor SSE", "", ByteSpan{0, len(source)}, false))
	}
	projection.Validation = validateProjection(projection)
	return projection
}

func InspectResponses(source []byte, format ...BodyFormat) ResponsesProjection {
	return InspectProtocol(ProtocolResponses, source, format...)
}
func InspectResponsesJSON(source []byte) ResponsesProjection {
	return InspectProtocol(ProtocolResponses, source, FormatJSON)
}
func InspectResponsesSSE(source []byte) ResponsesProjection {
	return InspectProtocol(ProtocolResponses, source, FormatSSE)
}
func InspectChatCompletions(source []byte, format ...BodyFormat) ChatCompletionsProjection {
	return InspectProtocol(ProtocolChatCompletions, source, format...)
}
func InspectChatCompletionsJSON(source []byte) ChatCompletionsProjection {
	return InspectProtocol(ProtocolChatCompletions, source, FormatJSON)
}
func InspectChatCompletionsSSE(source []byte) ChatCompletionsProjection {
	return InspectProtocol(ProtocolChatCompletions, source, FormatSSE)
}
func InspectAnthropicMessages(source []byte, format ...BodyFormat) AnthropicMessagesProjection {
	return InspectProtocol(ProtocolAnthropicMessages, source, format...)
}
func InspectAnthropicMessagesJSON(source []byte) AnthropicMessagesProjection {
	return InspectProtocol(ProtocolAnthropicMessages, source, FormatJSON)
}
func InspectAnthropicMessagesSSE(source []byte) AnthropicMessagesProjection {
	return InspectProtocol(ProtocolAnthropicMessages, source, FormatSSE)
}

// ValidateProtocol validates a body without retaining a second projection in
// the result.  The optional format follows InspectProtocol's inference rules.
func ValidateProtocol(protocol Protocol, source []byte, format ...BodyFormat) ValidationResult {
	return InspectProtocol(protocol, source, format...).Validation
}
func ValidateResponses(source []byte, format ...BodyFormat) ValidationResult {
	return ValidateProtocol(ProtocolResponses, source, format...)
}
func ValidateResponsesJSON(source []byte) ValidationResult {
	return ValidateProtocol(ProtocolResponses, source, FormatJSON)
}
func ValidateResponsesSSE(source []byte) ValidationResult {
	return ValidateProtocol(ProtocolResponses, source, FormatSSE)
}
func ValidateChatCompletions(source []byte, format ...BodyFormat) ValidationResult {
	return ValidateProtocol(ProtocolChatCompletions, source, format...)
}
func ValidateChatCompletionsJSON(source []byte) ValidationResult {
	return ValidateProtocol(ProtocolChatCompletions, source, FormatJSON)
}
func ValidateChatCompletionsSSE(source []byte) ValidationResult {
	return ValidateProtocol(ProtocolChatCompletions, source, FormatSSE)
}
func ValidateAnthropicMessages(source []byte, format ...BodyFormat) ValidationResult {
	return ValidateProtocol(ProtocolAnthropicMessages, source, format...)
}
func ValidateAnthropicMessagesJSON(source []byte) ValidationResult {
	return ValidateProtocol(ProtocolAnthropicMessages, source, FormatJSON)
}
func ValidateAnthropicMessagesSSE(source []byte) ValidationResult {
	return ValidateProtocol(ProtocolAnthropicMessages, source, FormatSSE)
}

func inferBodyFormat(source []byte) BodyFormat {
	trimmed := strings.TrimSpace(string(source))
	if trimmed == "" {
		return FormatUnknown
	}
	if bodyLooksLikeSSE(source) {
		return FormatSSE
	}
	var value any
	if json.Unmarshal(source, &value) == nil {
		return FormatJSON
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return FormatJSON
	}
	return FormatUnknown
}

func protocolSchema(protocol Protocol) JSONSchema {
	return func(pointer, key string) bool {
		return knownProtocolField(protocol, pointer, key)
	}
}

func protocolEventSchema(protocol Protocol) JSONSchema {
	return func(pointer, key string) bool {
		if pointer == "" {
			return containsString([]string{"type", "object", "sequence_number", "response", "item_id", "output_index", "content_index", "summary_index", "index", "delta", "part", "item", "payload", "content_block", "message", "usage", "error", "ping_id", "stop_reason", "stop_sequence", "output_tokens", "input_tokens"}, key) || knownProtocolField(protocol, pointer, key)
		}
		return knownProtocolField(protocol, pointer, key)
	}
}

// knownProtocolField deliberately covers protocol-defined fields at the
// meaningful envelope/content boundaries. Dynamic maps such as metadata,
// tool parameter schemas, and provider extensions remain lossless while their
// unknown members are not mistaken for a transport error.
func knownProtocolField(protocol Protocol, pointer, key string) bool {
	// Event payloads use a small common envelope across protocol streams. Keep
	// these fields known so inspectors can reserve unknown diagnostics for real
	// extensions rather than every event attribute.
	if strings.HasPrefix(pointer, "/events/") || strings.HasPrefix(pointer, "/data/events/") {
		return containsString([]string{"type", "object", "sequence_number", "response", "item_id", "output_index", "content_index", "summary_index", "index", "delta", "part", "item", "payload", "content_block", "message", "usage", "error", "ping_id", "stop_reason", "stop_sequence", "output_tokens", "input_tokens"}, key)
	}
	// These are open maps by protocol definition. A user/provider may put
	// arbitrary keys under metadata, tool input, JSON schema properties, or
	// extension payloads; retain them without false unknown-field warnings.
	if strings.Contains(pointer, "/metadata") || strings.Contains(pointer, "/parameters/properties") || strings.Contains(pointer, "/input_schema/properties") || strings.HasSuffix(pointer, "/input") || strings.HasSuffix(pointer, "/output") {
		return true
	}
	if pointer == "" {
		switch protocol {
		case ProtocolResponses:
			return containsString([]string{"id", "object", "created_at", "status", "model", "input", "instructions", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "text", "metadata", "stream", "store", "include", "output", "usage", "error", "previous_response_id", "conversation", "truncation", "max_output_tokens", "temperature", "top_p", "service_tier"}, key)
		case ProtocolChatCompletions:
			return containsString([]string{"id", "object", "created", "model", "messages", "tools", "tool_choice", "parallel_tool_calls", "stream", "stream_options", "temperature", "top_p", "response_format", "max_tokens", "max_completion_tokens", "audio", "logprobs", "n", "presence_penalty", "frequency_penalty", "stop", "seed", "user", "service_tier", "usage", "choices", "system_fingerprint", "error"}, key)
		case ProtocolAnthropicMessages:
			return containsString([]string{"id", "type", "role", "model", "max_tokens", "messages", "system", "tools", "tool_choice", "thinking", "metadata", "output_config", "service_tier", "stream", "content", "stop_reason", "stop_sequence", "usage", "container", "error"}, key)
		}
		return true
	}
	// Arrays of protocol objects. Keep standard fields known while marking
	// provider extensions as UnknownNodes at each item.
	switch protocol {
	case ProtocolResponses:
		if pointer == "/output" || strings.HasPrefix(pointer, "/output/") || strings.HasPrefix(pointer, "/input/") || strings.HasPrefix(pointer, "/tools/") {
			return containsString([]string{"id", "type", "status", "role", "content", "summary", "encrypted_content", "call_id", "name", "arguments", "output", "input", "description", "parameters", "text", "annotations", "format", "properties", "required", "additionalProperties", "detail", "image_url", "sequence_number", "item_id", "output_index", "content_index", "summary_index", "delta", "part", "response", "payload", "model"}, key)
		}
	case ProtocolChatCompletions:
		if pointer == "/choices" || strings.HasPrefix(pointer, "/choices/") {
			return containsString([]string{"index", "message", "delta", "finish_reason", "logprobs", "role", "content", "tool_calls", "function", "refusal", "id", "type", "name", "arguments"}, key)
		}
		if pointer == "/messages" || strings.HasPrefix(pointer, "/messages/") || pointer == "/tools" || strings.HasPrefix(pointer, "/tools/") {
			return containsString([]string{"role", "content", "name", "tool_calls", "tool_call_id", "function", "id", "type", "arguments", "description", "parameters", "strict", "image_url", "url", "detail", "text", "audio", "refusal"}, key)
		}
	case ProtocolAnthropicMessages:
		if pointer == "/content" || strings.HasPrefix(pointer, "/content/") || strings.Contains(pointer, "/content/") || strings.HasPrefix(pointer, "/messages/") || strings.HasPrefix(pointer, "/system/") {
			return containsString([]string{"type", "text", "thinking", "signature", "data", "id", "name", "input", "tool_use_id", "content", "is_error", "source", "media_type", "cache_control", "citations", "index", "delta", "stop_reason", "stop_sequence", "message", "usage", "output_tokens", "input_tokens", "role"}, key)
		}
	}
	// Objects not at a protocol boundary are retained without labelling every
	// dynamic key as unknown. Unknown top-level/content keys remain visible.
	return true
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func lastPointerToken(pointer string) string {
	if pointer == "" {
		return ""
	}
	parts := strings.Split(pointer, "/")
	if len(parts) == 0 {
		return ""
	}
	value := parts[len(parts)-1]
	value = strings.ReplaceAll(strings.ReplaceAll(value, "~1", "/"), "~0", "~")
	return value
}

func populateJSONSections(projection *ProtocolProjection) {
	root := projection.Root
	if root == nil || root.Kind != JSONObject {
		projection.Kind = PayloadUnknown
		return
	}
	projection.Kind = classifyJSONKind(projection.Protocol, root)
	projection.Model = objectFieldNode(root, "model")
	projection.Input = objectFieldNode(root, "input")
	projection.Instructions = objectFieldNode(root, "instructions")
	projection.Messages = objectFieldNode(root, "messages")
	projection.System = objectFieldNode(root, "system")
	projection.Tools = objectFieldNode(root, "tools")
	projection.ToolChoice = objectFieldNode(root, "tool_choice")
	projection.Output = objectFieldNode(root, "output")
	projection.Choices = objectFieldNode(root, "choices")
	projection.Usage = objectFieldNode(root, "usage")
	if projection.Usage != nil {
		projection.UsageItems = append(projection.UsageItems, projection.Usage)
	}
	if projection.Output != nil && projection.Output.Kind == JSONArray {
		for _, item := range projection.Output.Items {
			appendResponsesOutputItem(projection, item)
		}
	}
	if projection.Choices != nil && projection.Choices.Kind == JSONArray {
		for ordinal, choice := range projection.Choices.Items {
			item := ProtocolChoice{Ordinal: ordinal, Node: choice}
			item.Index = integerNodeField(choice, "index")
			item.Message = objectFieldNode(choice, "message")
			item.Delta = objectFieldNode(choice, "delta")
			item.FinishReason = objectFieldNode(choice, "finish_reason")
			item.Logprobs = objectFieldNode(choice, "logprobs")
			projection.ChoiceItems = append(projection.ChoiceItems, item)
		}
	}
	collectContentSections(projection, root)
}

func populateSSESections(projection *ProtocolProjection, protocol Protocol) {
	if projection.SSE == nil {
		return
	}
	projection.Kind = PayloadStream
	terminal := false
	for ordinal, event := range projection.SSE.Events {
		pe := ProtocolEvent{
			Ordinal:  ordinal,
			Name:     event.Name,
			Data:     event.Data,
			Raw:      cloneBytes(event.Raw),
			Span:     event.Span,
			Complete: event.Complete,
		}
		trimmed := strings.TrimSpace(event.Data)
		if trimmed == "[DONE]" {
			pe.DoneSentinel = true
			projection.SawDoneSentinel = true
			if protocol == ProtocolChatCompletions {
				terminal = true
			} else {
				projection.Warnings = append(projection.Warnings, warning("unexpected_done_sentinel", "[DONE] is a Chat Completions sentinel and is not a protocol terminator here", "/events/"+strconv.Itoa(ordinal), event.Span, false))
			}
			projection.Events = append(projection.Events, pe)
			continue
		}
		jsonProjection := InspectJSONWithSchema([]byte(event.Data), protocolEventSchema(protocol))
		pe.JSON = &jsonProjection
		pe.Payload = jsonProjection.Root
		pe.UnknownNodes = cloneUnknownNodes(jsonProjection.UnknownNodes)
		for _, item := range jsonProjection.Warnings {
			item.Fatal = false // malformed event data never invalidates raw SSE bypass
			item.Pointer = "/events/" + strconv.Itoa(ordinal) + "/data" + item.Pointer
			pe.Warnings = append(pe.Warnings, item)
			projection.Warnings = append(projection.Warnings, item)
		}
		for _, item := range pe.UnknownNodes {
			item.Pointer = "/events/" + strconv.Itoa(ordinal) + "/data" + item.Pointer
			projection.UnknownNodes = append(projection.UnknownNodes, item)
		}
		if pe.Payload != nil && pe.Payload.Kind == JSONObject {
			pe.Type = stringNodeField(pe.Payload, "type")
			if pe.Type == "" {
				pe.Type = event.Name
			}
			pe.SequenceNumber = integerNodeField(pe.Payload, "sequence_number")
			pe.ContentBlockIndex = integerNodeField(pe.Payload, "index")
			if pe.SequenceNumber != nil {
				projection.SequenceNumbers = append(projection.SequenceNumbers, *pe.SequenceNumber)
			}
			if pe.ContentBlockIndex != nil {
				projection.ContentBlockIndices = append(projection.ContentBlockIndices, *pe.ContentBlockIndex)
			}
			if usage := objectFieldNode(pe.Payload, "usage"); usage != nil {
				projection.UsageItems = append(projection.UsageItems, usage)
			}
			// Anthropic message_start carries the authoritative input usage on
			// its embedded message object.
			if message := objectFieldNode(pe.Payload, "message"); message != nil {
				if usage := objectFieldNode(message, "usage"); usage != nil {
					projection.UsageItems = append(projection.UsageItems, usage)
				}
			}
			if output := objectFieldNode(pe.Payload, "output"); output != nil && output.Kind == JSONArray {
				for _, item := range output.Items {
					appendResponsesOutputItem(projection, item)
				}
			}
			if response := objectFieldNode(pe.Payload, "response"); response != nil && response.Kind == JSONObject {
				if projection.Model == nil {
					projection.Model = objectFieldNode(response, "model")
				}
				if output := objectFieldNode(response, "output"); output != nil && output.Kind == JSONArray {
					for _, item := range output.Items {
						appendResponsesOutputItem(projection, item)
					}
				}
				if usage := objectFieldNode(response, "usage"); usage != nil {
					projection.UsageItems = append(projection.UsageItems, usage)
				}
			}
			if item := objectFieldNode(pe.Payload, "item"); item != nil {
				appendResponsesOutputItem(projection, item)
			}
			if block := objectFieldNode(pe.Payload, "content_block"); block != nil {
				projection.ContentBlocks = append(projection.ContentBlocks, block)
				classifyContentBlock(projection, block)
			}
			if delta := objectFieldNode(pe.Payload, "delta"); delta != nil {
				if stringNodeField(delta, "type") != "" {
					projection.ContentBlocks = append(projection.ContentBlocks, delta)
					classifyContentBlock(projection, delta)
				}
				if block := objectFieldNode(delta, "content_block"); block != nil {
					projection.ContentBlocks = append(projection.ContentBlocks, block)
					classifyContentBlock(projection, block)
				}
			}
			if event.Name == "message_stop" || strings.HasSuffix(pe.Type, ".completed") || strings.HasSuffix(pe.Type, ".failed") || strings.HasSuffix(pe.Type, ".incomplete") {
				terminal = true
			}
		}
		projection.Events = append(projection.Events, pe)
	}
	if protocol == ProtocolResponses {
		// Sequence numbers are monotonic observations, not a completion rule.
		for i := 1; i < len(projection.SequenceNumbers); i++ {
			if projection.SequenceNumbers[i] < projection.SequenceNumbers[i-1] {
				projection.Warnings = append(projection.Warnings, warning("sequence_number_regressed", "Responses sequence_number regressed", "/events/"+strconv.Itoa(i), ByteSpan{}, false))
			}
		}
	}
	projection.ProtocolComplete = terminal
	if len(projection.Warnings) > 0 && projection.Status == ParseOK {
		projection.Status = ParsePartial
	}
}

func appendResponsesOutputItem(projection *ProtocolProjection, item *JSONNode) {
	if item == nil {
		return
	}
	projection.OutputItems = append(projection.OutputItems, item)
	typeName := strings.ToLower(stringNodeField(item, "type"))
	switch {
	case typeName == "reasoning" || strings.HasPrefix(typeName, "reasoning_"):
		projection.ReasoningItems = append(projection.ReasoningItems, item)
	case strings.Contains(typeName, "call") || strings.Contains(typeName, "tool"):
		projection.ToolCallItems = append(projection.ToolCallItems, item)
	}
}

func classifyJSONKind(protocol Protocol, root *JSONNode) PayloadKind {
	object := strings.ToLower(stringNodeField(root, "object"))
	typeName := strings.ToLower(stringNodeField(root, "type"))
	switch protocol {
	case ProtocolResponses:
		_, hasOutput := root.Field("output")
		_, hasStatus := root.Field("status")
		if object == "response" || hasOutput && hasStatus {
			return PayloadResponse
		}
		if typeName != "" && strings.HasPrefix(typeName, "response") {
			return PayloadEvent
		}
		return PayloadRequest
	case ProtocolChatCompletions:
		_, hasChoices := root.Field("choices")
		if object == "chat.completion" || object == "chat.completion.chunk" || hasChoices {
			return PayloadResponse
		}
		return PayloadRequest
	case ProtocolAnthropicMessages:
		_, hasStopReason := root.Field("stop_reason")
		if typeName == "message" || hasStopReason {
			return PayloadResponse
		}
		return PayloadRequest
	default:
		return PayloadUnknown
	}
}

func objectFieldNode(node *JSONNode, key string) *JSONNode {
	if node == nil || node.Kind != JSONObject {
		return nil
	}
	field, ok := node.Field(key)
	if !ok {
		return nil
	}
	return field.Value
}

func stringNodeField(node *JSONNode, key string) string {
	value := objectFieldNode(node, key)
	if value == nil || value.Kind != JSONString {
		return ""
	}
	text, _ := value.Value.(string)
	return text
}

func integerNodeField(node *JSONNode, key string) *int64 {
	value := objectFieldNode(node, key)
	if value == nil || value.Kind != JSONNumber {
		return nil
	}
	text := string(value.Raw)
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func collectContentSections(projection *ProtocolProjection, root *JSONNode) {
	if root == nil {
		return
	}
	var walk func(*JSONNode)
	walk = func(node *JSONNode) {
		if node == nil {
			return
		}
		if node.Kind == JSONObject {
			typeName := strings.ToLower(stringNodeField(node, "type"))
			if typeName != "" {
				if strings.Contains(node.Pointer, "/content") || strings.HasSuffix(node.Pointer, "/content_block") || strings.Contains(node.Pointer, "/delta") {
					if !containsNodePointer(projection.ContentBlocks, node.Pointer) {
						projection.ContentBlocks = append(projection.ContentBlocks, node)
					}
					classifyContentBlock(projection, node)
				}
			}
			for _, field := range node.Fields {
				walk(field.Value)
			}
		} else if node.Kind == JSONArray {
			for _, item := range node.Items {
				walk(item)
			}
		}
	}
	walk(root)
}

func classifyContentBlock(projection *ProtocolProjection, node *JSONNode) {
	if node == nil {
		return
	}
	typeName := strings.ToLower(stringNodeField(node, "type"))
	switch typeName {
	case "thinking", "thinking_delta":
		if !containsNodePointer(projection.ThinkingBlocks, node.Pointer) {
			projection.ThinkingBlocks = append(projection.ThinkingBlocks, node)
		}
	case "redacted_thinking":
		if !containsNodePointer(projection.RedactedThinking, node.Pointer) {
			projection.RedactedThinking = append(projection.RedactedThinking, node)
		}
	case "tool_use", "tool_use_delta", "function_call", "function_call_output":
		if !containsNodePointer(projection.ToolUseBlocks, node.Pointer) {
			projection.ToolUseBlocks = append(projection.ToolUseBlocks, node)
		}
	case "tool_result":
		if !containsNodePointer(projection.ToolResultBlocks, node.Pointer) {
			projection.ToolResultBlocks = append(projection.ToolResultBlocks, node)
		}
	}
	if signature := objectFieldNode(node, "signature"); signature != nil && !containsNodePointer(projection.SignatureNodes, signature.Pointer) {
		projection.SignatureNodes = append(projection.SignatureNodes, signature)
	}
}

func containsNodePointer(nodes []*JSONNode, pointer string) bool {
	for _, node := range nodes {
		if node != nil && node.Pointer == pointer {
			return true
		}
	}
	return false
}

func validateProjection(projection ProtocolProjection) ValidationResult {
	result := ValidationResult{Protocol: projection.Protocol, Format: projection.Format, Kind: projection.Kind, Valid: true}
	addError := func(code, message, pointer string, span ByteSpan) {
		result.Errors = append(result.Errors, ValidationIssue{Code: code, Message: message, Pointer: pointer, Span: span, Fatal: true})
	}
	addWarning := func(code, message, pointer string, span ByteSpan) {
		result.Warnings = append(result.Warnings, ValidationIssue{Code: code, Message: message, Pointer: pointer, Span: span})
	}
	if projection.Format == FormatUnknown {
		addError("unknown_format", "body format is unknown", "", ByteSpan{0, len(projection.Source)})
	}
	if projection.JSON != nil {
		if !projection.JSON.Valid || projection.Root == nil || projection.Root.Kind != JSONObject {
			addError("invalid_json", "protocol body must be a valid JSON object", "", rootSpan(projection.Root))
		}
		for _, item := range projection.JSON.Warnings {
			if item.Fatal {
				// Avoid duplicating a generic parse error; one protocol error is
				// enough while preserving the original warning in projection.
				continue
			}
			addWarning(item.Code, item.Message, item.Pointer, item.Span)
		}
		if projection.Root != nil && projection.Root.Kind == JSONObject {
			switch projection.Protocol {
			case ProtocolResponses:
				validateResponsesJSON(&result, projection.Root, addError, addWarning)
			case ProtocolChatCompletions:
				validateChatJSON(&result, projection.Root, addError, addWarning)
			case ProtocolAnthropicMessages:
				validateAnthropicJSON(&result, projection.Root, addError, addWarning)
			}
		}
	}
	if projection.SSE != nil {
		validateSSE(&result, projection, addError, addWarning)
	}
	result.Valid = len(result.Errors) == 0
	return result
}

func validateResponsesJSON(result *ValidationResult, root *JSONNode, addError func(string, string, string, ByteSpan), addWarning func(string, string, string, ByteSpan)) {
	if projectionLooksResponse(root, ProtocolResponses) {
		if value := objectFieldNode(root, "output"); value != nil && value.Kind != JSONArray {
			addError("output_type", "Responses output must be an array", "/output", value.Span)
		}
		return
	}
	if model := objectFieldNode(root, "model"); model == nil || model.Kind != JSONString || strings.TrimSpace(stringNodeField(root, "model")) == "" {
		addError("model_required", "Responses request requires a non-empty model", "/model", spanOf(objectFieldNode(root, "model")))
	}
	if input := objectFieldNode(root, "input"); input != nil && input.Kind != JSONString && input.Kind != JSONArray {
		addError("input_type", "Responses input must be a string or array", "/input", input.Span)
	}
	if stream := objectFieldNode(root, "stream"); stream != nil && stream.Kind != JSONBoolean {
		addError("stream_type", "Responses stream must be boolean", "/stream", stream.Span)
	}
	_ = addWarning
}

func validateChatJSON(result *ValidationResult, root *JSONNode, addError func(string, string, string, ByteSpan), addWarning func(string, string, string, ByteSpan)) {
	if projectionLooksResponse(root, ProtocolChatCompletions) {
		choices := objectFieldNode(root, "choices")
		if choices == nil || choices.Kind != JSONArray {
			addError("choices_required", "Chat Completions response requires choices array", "/choices", spanOf(choices))
		}
		return
	}
	model := objectFieldNode(root, "model")
	if model == nil || model.Kind != JSONString || strings.TrimSpace(stringNodeField(root, "model")) == "" {
		addError("model_required", "Chat Completions request requires a non-empty model", "/model", spanOf(model))
	}
	messages := objectFieldNode(root, "messages")
	if messages == nil || messages.Kind != JSONArray {
		addError("messages_required", "Chat Completions request requires messages array", "/messages", spanOf(messages))
	}
	_ = addWarning
}

func validateAnthropicJSON(result *ValidationResult, root *JSONNode, addError func(string, string, string, ByteSpan), addWarning func(string, string, string, ByteSpan)) {
	if projectionLooksResponse(root, ProtocolAnthropicMessages) {
		content := objectFieldNode(root, "content")
		if content == nil || content.Kind != JSONArray {
			addError("content_required", "Anthropic Message response requires content array", "/content", spanOf(content))
		}
		return
	}
	model := objectFieldNode(root, "model")
	if model == nil || model.Kind != JSONString || strings.TrimSpace(stringNodeField(root, "model")) == "" {
		addError("model_required", "Anthropic Messages request requires a non-empty model", "/model", spanOf(model))
	}
	maxTokens := objectFieldNode(root, "max_tokens")
	if maxTokens == nil || maxTokens.Kind != JSONNumber {
		addError("max_tokens_required", "Anthropic Messages request requires numeric max_tokens", "/max_tokens", spanOf(maxTokens))
	}
	messages := objectFieldNode(root, "messages")
	if messages == nil || messages.Kind != JSONArray {
		addError("messages_required", "Anthropic Messages request requires messages array", "/messages", spanOf(messages))
	}
	_ = addWarning
}

func projectionLooksResponse(root *JSONNode, protocol Protocol) bool {
	if root == nil {
		return false
	}
	switch protocol {
	case ProtocolResponses:
		return strings.EqualFold(stringNodeField(root, "object"), "response") || objectFieldNode(root, "output") != nil && objectFieldNode(root, "status") != nil
	case ProtocolChatCompletions:
		return strings.HasPrefix(strings.ToLower(stringNodeField(root, "object")), "chat.completion") || objectFieldNode(root, "choices") != nil
	case ProtocolAnthropicMessages:
		return strings.EqualFold(stringNodeField(root, "type"), "message") || objectFieldNode(root, "stop_reason") != nil
	default:
		return false
	}
}

func validateSSE(result *ValidationResult, projection ProtocolProjection, addError func(string, string, string, ByteSpan), addWarning func(string, string, string, ByteSpan)) {
	if projection.SSE == nil {
		return
	}
	if len(projection.SSE.Events) == 0 {
		addWarning("empty_stream", "SSE stream contains no data events", "/events", ByteSpan{0, len(projection.Source)})
	}
	seenTerminal := false
	for i, event := range projection.Events {
		pointer := "/events/" + strconv.Itoa(i)
		if event.DoneSentinel {
			if projection.Protocol == ProtocolChatCompletions {
				seenTerminal = true
			} else {
				addError("done_sentinel_forbidden", "[DONE] is not a terminator for this protocol", pointer, event.Span)
			}
			continue
		}
		if event.JSON == nil || event.Payload == nil || event.Payload.Kind != JSONObject {
			addWarning("event_json_unavailable", "SSE event data is not a JSON object", pointer+"/data", event.Span)
			continue
		}
		switch projection.Protocol {
		case ProtocolResponses:
			if event.Type != "" && !strings.HasPrefix(strings.ToLower(event.Type), "response") {
				addWarning("responses_event_type", "Responses event type is outside the response namespace", pointer+"/data/type", event.Span)
			}
			if event.SequenceNumber == nil {
				addWarning("sequence_number_missing", "Responses events normally carry sequence_number", pointer+"/data/sequence_number", event.Span)
			}
			if strings.HasSuffix(strings.ToLower(event.Type), ".completed") || strings.HasSuffix(strings.ToLower(event.Type), ".failed") || strings.HasSuffix(strings.ToLower(event.Type), ".incomplete") {
				seenTerminal = true
			}
		case ProtocolChatCompletions:
			object := strings.ToLower(stringNodeField(event.Payload, "object"))
			if object != "" && object != "chat.completion.chunk" {
				addWarning("chat_chunk_object", "Chat SSE event object is not chat.completion.chunk", pointer+"/data/object", event.Span)
			}
			if choices := objectFieldNode(event.Payload, "choices"); choices != nil && choices.Kind != JSONArray {
				addError("choices_type", "Chat SSE choices must be an array", pointer+"/data/choices", choices.Span)
			}
		case ProtocolAnthropicMessages:
			if event.Name == "" {
				addWarning("anthropic_event_name_missing", "Anthropic SSE records should name their event", pointer, event.Span)
			}
			if event.Name == "content_block_start" || event.Name == "content_block_delta" || event.Name == "content_block_stop" {
				if event.ContentBlockIndex == nil {
					addError("content_block_index_required", "Anthropic content block event requires non-negative index", pointer+"/data/index", event.Span)
				} else if *event.ContentBlockIndex < 0 {
					addError("content_block_index_negative", "Anthropic content block index cannot be negative", pointer+"/data/index", event.Span)
				}
			}
			if event.Name == "message_stop" {
				seenTerminal = true
			}
		}
	}
	if projection.SSE.Complete && !seenTerminal {
		// A complete capture without a protocol terminal record is useful to
		// inspect, but must not be mistaken for a completed model response.
		addWarning("terminal_event_missing", "stream ended without a protocol terminal event", "/events", ByteSpan{0, len(projection.Source)})
	}
	if projection.Protocol == ProtocolResponses && projection.SawDoneSentinel {
		addError("responses_done_sentinel", "Responses streams do not use [DONE] as a completion rule", "/events", ByteSpan{0, len(projection.Source)})
	}
}

func rootSpan(root *JSONNode) ByteSpan {
	if root == nil {
		return ByteSpan{}
	}
	return root.Span
}
func spanOf(node *JSONNode) ByteSpan {
	if node == nil {
		return ByteSpan{}
	}
	return node.Span
}
func cloneUnknownNodes(values []UnknownNode) []UnknownNode {
	if len(values) == 0 {
		return nil
	}
	out := make([]UnknownNode, len(values))
	for i, item := range values {
		out[i] = item
		out[i].Raw = cloneBytes(item.Raw)
	}
	return out
}
func cloneWarnings(values []Warning) []Warning {
	if len(values) == 0 {
		return nil
	}
	return append([]Warning(nil), values...)
}
