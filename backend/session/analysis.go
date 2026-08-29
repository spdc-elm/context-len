package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"context-lens/backend/inspection"
)

// RequestAnalysis is the complete capture-time observation of one request
// body: the queue summary plus the facts the session index needs to place
// the request in a session tree. It is derived once from the original inbound
// bytes and never becomes transport input.
type RequestAnalysis struct {
	Summary            Summary
	MessageDigests     []string
	ToolsDigest        string
	PreviousResponseID string
}

// RequestFacts is the subset of RequestAnalysis consumed by Index.Assign.
type RequestFacts struct {
	Protocol           inspection.Protocol
	MessageDigests     []string
	Model              string
	ToolsDigest        string
	PreviousResponseID string
}

// AnalyzeRequest derives the summary projection and the session-identity
// facts from an original inbound request body. It never fails: unparseable or
// non-chat bodies yield zero-value facts the caller may drop.
func AnalyzeRequest(protocol inspection.Protocol, body []byte) RequestAnalysis {
	analysis := RequestAnalysis{}
	if !IsChatProtocol(protocol) {
		return analysis
	}
	root := inspection.InspectJSON(body).Root
	if root == nil || root.Kind != inspection.JSONObject {
		return analysis
	}
	analysis.Summary = summarizeRequest(protocol, root)
	analysis.MessageDigests = messageDigests(protocol, root)
	analysis.ToolsDigest = nodeDigest(arrayField(root, "tools"))
	if protocol == inspection.ProtocolResponses {
		analysis.PreviousResponseID = stringField(root, "previous_response_id")
	}
	return analysis
}

// ExtractResponseID reads the response identifier of a Responses body: the
// top-level id for JSON and the first event response.id for SSE. Other
// protocols return an empty string; response ids are only used for the
// previous_response_id continuation seam.
func ExtractResponseID(body []byte) string {
	projection := inspection.InspectProtocol(inspection.ProtocolResponses, body)
	if projection.Root != nil && projection.Root.Kind == inspection.JSONObject {
		if id := stringField(projection.Root, "id"); id != "" {
			return id
		}
	}
	for _, event := range projection.Events {
		if event.Payload == nil || event.Payload.Kind != inspection.JSONObject {
			continue
		}
		if response := objectField(event.Payload, "response"); response != nil {
			if id := stringField(response, "id"); id != "" {
				return id
			}
		}
	}
	return ""
}

// SummarizeRequest computes only the queue summary fields.
func SummarizeRequest(protocol inspection.Protocol, body []byte) Summary {
	if !IsChatProtocol(protocol) {
		return Summary{}
	}
	root := inspection.InspectJSON(body).Root
	if root == nil || root.Kind != inspection.JSONObject {
		return Summary{}
	}
	return summarizeRequest(protocol, root)
}

func summarizeRequest(protocol inspection.Protocol, root *inspection.JSONNode) Summary {
	summary := Summary{}
	summary.Model = stringField(root, "model")
	summary.MessageCount = messageSequenceLength(protocol, root)
	summary.ToolNames = toolNames(protocol, root)
	summary.Preview = requestPreview(protocol, root, summary.Model, summary.MessageCount)
	return summary
}

// messageDigests returns one digest per element of the normalized message
// sequence (virtual system element first). Digests are computed from a
// canonical serialization so key order and escape-spelling drift cannot break
// chain identity, while numeric spelling stays significant (1 and 1.0 are
// deliberately different messages).
func messageDigests(protocol inspection.Protocol, root *inspection.JSONNode) []string {
	var digests []string
	if system := systemElement(protocol, root); system != nil {
		digests = append(digests, nodeDigest(system))
	}
	switch protocol {
	case inspection.ProtocolChatCompletions, inspection.ProtocolAnthropicMessages:
		if messages := arrayField(root, "messages"); messages != nil {
			for _, message := range messages.Items {
				digests = append(digests, nodeDigest(message))
			}
		}
	case inspection.ProtocolResponses:
		if input := objectField(root, "input"); input != nil {
			if input.Kind == inspection.JSONArray {
				for _, item := range input.Items {
					digests = append(digests, nodeDigest(item))
				}
			} else {
				digests = append(digests, nodeDigest(input))
			}
		}
	}
	return digests
}

// nodeDigest returns the hex SHA-256 of a node's canonical serialization.
func nodeDigest(node *inspection.JSONNode) string {
	if node == nil {
		return ""
	}
	sum := sha256.Sum256(canonicalBytes(node))
	return hex.EncodeToString(sum[:])
}

// canonicalBytes serializes a JSON node deterministically: object fields are
// sorted by decoded key, strings use one standard escaping, and numbers keep
// their original spelling.
func canonicalBytes(node *inspection.JSONNode) []byte {
	var builder strings.Builder
	writeCanonical(&builder, node)
	return []byte(builder.String())
}

func writeCanonical(builder *strings.Builder, node *inspection.JSONNode) {
	if node == nil {
		builder.WriteString("null")
		return
	}
	switch node.Kind {
	case inspection.JSONObject:
		fields := make([]inspection.JSONField, len(node.Fields))
		copy(fields, node.Fields)
		sort.SliceStable(fields, func(i, j int) bool { return fields[i].Key < fields[j].Key })
		builder.WriteByte('{')
		for i, field := range fields {
			if i > 0 {
				builder.WriteByte(',')
			}
			builder.WriteString(quoteJSON(field.Key))
			builder.WriteByte(':')
			writeCanonical(builder, field.Value)
		}
		builder.WriteByte('}')
	case inspection.JSONArray:
		builder.WriteByte('[')
		for i, item := range node.Items {
			if i > 0 {
				builder.WriteByte(',')
			}
			writeCanonical(builder, item)
		}
		builder.WriteByte(']')
	case inspection.JSONString:
		text, _ := node.Value.(string)
		builder.WriteString(quoteJSON(text))
	case inspection.JSONNumber:
		builder.Write(node.Raw)
	case inspection.JSONBoolean, inspection.JSONNull:
		builder.Write(node.Raw)
	default:
		builder.Write(node.Raw)
	}
}

// quoteJSON writes one standard JSON string literal with fixed-width
// control-character escapes.
func quoteJSON(text string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range text {
		switch r {
		case '"':
			builder.WriteString("\\\"")
		case '\\':
			builder.WriteString("\\\\")
		case '\n':
			builder.WriteString("\\n")
		case '\r':
			builder.WriteString("\\r")
		case '\t':
			builder.WriteString("\\t")
		case '\b':
			builder.WriteString("\\b")
		case '\f':
			builder.WriteString("\\f")
		default:
			if r < 0x20 {
				builder.WriteString(fmt.Sprintf("\\u%04x", r))
			} else {
				builder.WriteRune(r)
			}
		}
	}
	builder.WriteByte('"')
	return builder.String()
}
