package inspection

import (
	"encoding/json"
	"net/http"
	"strings"
)

// HintProtocol determines a protocol and body format from request metadata and
// a body sample.  Path and explicit provider headers are strongest signals;
// body fields are fallback evidence and are never rewritten.  It is safe to
// call this on partial or malformed bodies.
func HintProtocol(input HintInput) ProtocolHint {
	path := normalizedPath(input.Path)
	contentType := input.ContentType
	if contentType == "" {
		contentType = HeaderValue(input.Headers, "Content-Type")
	}
	contentType = strings.ToLower(contentType)
	body := input.Body
	format := FormatUnknown
	if strings.Contains(contentType, "text/event-stream") || bodyLooksLikeSSE(body) {
		format = FormatSSE
	} else if strings.TrimSpace(string(body)) != "" {
		// A JSON probe is deliberately shallow here.  Full parsing belongs to
		// InspectJSON, whose warnings can be attached to a projection.
		var value any
		if json.Unmarshal(body, &value) == nil {
			format = FormatJSON
		} else if strings.HasPrefix(strings.TrimSpace(string(body)), "{") || strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			format = FormatJSON
		}
	}

	result := ProtocolHint{Protocol: ProtocolUnknown, Format: format, Confidence: ConfidenceNone}
	add := func(protocol Protocol, confidence HintConfidence, evidence string) {
		if result.Protocol == ProtocolUnknown || confidenceRank(confidence) > confidenceRank(result.Confidence) {
			result.Protocol = protocol
			result.Confidence = confidence
			result.Evidence = nil
		}
		if result.Protocol == protocol {
			result.Evidence = append(result.Evidence, evidence)
		}
	}

	// The endpoint path is the least ambiguous signal and handles empty bodies
	// such as a request with a streaming body that has not arrived yet.
	switch {
	case pathMatches(path, "/v1/responses"):
		add(ProtocolResponses, ConfidenceHigh, "path matches /v1/responses")
	case pathMatches(path, "/v1/chat/completions"):
		add(ProtocolChatCompletions, ConfidenceHigh, "path matches /v1/chat/completions")
	case pathMatches(path, "/v1/messages"):
		add(ProtocolAnthropicMessages, ConfidenceHigh, "path matches /v1/messages")
	}

	// Explicit Anthropic headers are a strong provider signal.  Do not include
	// their values in evidence, as they can contain sensitive material.
	if HeaderValue(input.Headers, "anthropic-version") != "" || HeaderValue(input.Headers, "anthropic-beta") != "" || HeaderValue(input.Headers, "x-api-key") != "" {
		add(ProtocolAnthropicMessages, ConfidenceHigh, "Anthropic protocol header present")
	}

	var root map[string]any
	if format == FormatJSON {
		_ = json.Unmarshal(body, &root)
	}
	if format == FormatSSE {
		sse := InspectSSE(body)
		for _, event := range sse.Events {
			name := strings.ToLower(event.Name)
			data := strings.TrimSpace(event.Data)
			switch {
			case strings.HasPrefix(name, "response.") || strings.HasPrefix(name, "response_"):
				add(ProtocolResponses, ConfidenceMedium, "SSE event name uses Responses namespace")
			case isAnthropicSSEEvent(name):
				add(ProtocolAnthropicMessages, ConfidenceMedium, "SSE event name uses Anthropic Messages grammar")
			case data == "[DONE]":
				add(ProtocolChatCompletions, ConfidenceMedium, "SSE [DONE] sentinel observed")
			}
			if data == "[DONE]" {
				continue
			}
			var eventObject map[string]any
			if json.Unmarshal([]byte(data), &eventObject) == nil {
				addJSONSignals(&result, add, eventObject)
			}
		}
	} else if format == FormatJSON && root != nil {
		addJSONSignals(&result, add, root)
	}

	if result.Protocol == ProtocolUnknown && format == FormatJSON {
		result.Protocol = ProtocolGenericJSON
		result.Confidence = ConfidenceLow
		result.Evidence = append(result.Evidence, "valid JSON body without a recognised protocol shape")
	}
	if result.Protocol == ProtocolUnknown && format == FormatSSE {
		result.Confidence = ConfidenceLow
		result.Evidence = append(result.Evidence, "event-stream body without a recognised protocol shape")
	}
	if format == FormatUnknown && strings.TrimSpace(string(body)) != "" {
		result.Warnings = append(result.Warnings, warning("unknown_body_format", "body is neither recognisable JSON nor SSE", "", ByteSpan{0, len(body)}, false))
	}
	return result
}

// DetectProtocol is a convenience wrapper for callers that have separate path,
// content type, headers and body values.  Headers are copied by http.Header's
// read-only accessors and no values are included in the resulting evidence.
func DetectProtocol(path, contentType string, headers http.Header, body []byte) ProtocolHint {
	return HintProtocol(HintInput{Path: path, ContentType: contentType, Headers: headers, Body: body})
}

// Inspect chooses the generic JSON or SSE parser after producing a protocol
// hint.  This is still projection-only; no result is suitable as transport
// input and malformed input is returned with warnings.
func Inspect(input HintInput) Inspection {
	hint := HintProtocol(input)
	result := Inspection{Hint: hint}
	switch hint.Protocol {
	case ProtocolResponses, ProtocolChatCompletions, ProtocolAnthropicMessages:
		projection := InspectProtocol(hint.Protocol, input.Body, hint.Format)
		result.Protocol = &projection
		result.JSON = projection.JSON
		result.SSE = projection.SSE
		return result
	}
	switch hint.Format {
	case FormatJSON:
		projection := InspectJSON(input.Body)
		result.JSON = &projection
	case FormatSSE:
		projection := InspectSSE(input.Body)
		result.SSE = &projection
	}
	return result
}

func addJSONSignals(result *ProtocolHint, add func(Protocol, HintConfidence, string), value map[string]any) {
	if value == nil {
		return
	}
	if object, ok := value["object"].(string); ok {
		switch strings.ToLower(object) {
		case "chat.completion", "chat.completion.chunk":
			add(ProtocolChatCompletions, ConfidenceMedium, "JSON object identifies Chat Completions")
		case "response", "response.output_text.delta":
			add(ProtocolResponses, ConfidenceMedium, "JSON object identifies Responses")
		case "message":
			add(ProtocolAnthropicMessages, ConfidenceMedium, "JSON object identifies Anthropic Message")
		}
	}
	if typeName, ok := value["type"].(string); ok {
		lower := strings.ToLower(typeName)
		switch {
		case lower == "message" || strings.HasPrefix(lower, "message_") || strings.HasPrefix(lower, "content_block_"):
			add(ProtocolAnthropicMessages, ConfidenceMedium, "JSON type identifies Anthropic Messages")
		case lower == "response" || strings.HasPrefix(lower, "response.") || strings.HasPrefix(lower, "response_"):
			add(ProtocolResponses, ConfidenceMedium, "JSON type identifies Responses")
		}
	}
	_, hasInput := value["input"]
	_, hasInstructions := value["instructions"]
	if hasInput || hasInstructions || value["previous_response_id"] != nil || value["reasoning"] != nil {
		add(ProtocolResponses, ConfidenceLow, "Responses request field observed")
	}
	_, hasMessages := value["messages"]
	_, hasMaxTokens := value["max_tokens"]
	if hasMessages && hasMaxTokens {
		add(ProtocolAnthropicMessages, ConfidenceLow, "messages and max_tokens fields observed")
	} else if hasMessages {
		add(ProtocolChatCompletions, ConfidenceLow, "messages field observed")
	}
}

func pathMatches(path, endpoint string) bool {
	path = strings.TrimRight(strings.ToLower(path), "/")
	endpoint = strings.TrimRight(strings.ToLower(endpoint), "/")
	return path == endpoint || strings.HasSuffix(path, endpoint)
}

func confidenceRank(confidence HintConfidence) int {
	switch confidence {
	case ConfidenceHigh:
		return 3
	case ConfidenceMedium:
		return 2
	case ConfidenceLow:
		return 1
	default:
		return 0
	}
}

func isAnthropicSSEEvent(name string) bool {
	switch name {
	case "message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "ping", "error":
		return true
	default:
		return false
	}
}
