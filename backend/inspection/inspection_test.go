package inspection

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestInspectJSONRetainsBytesNumbersOrderAndPointers(t *testing.T) {
	source := []byte("{\n  \"model\": \"gpt-x\",\n  \"a/b\": [1.00, {\"x\":true,\"unknown\":null}],\n  \"model\": \"duplicate\"\n}\n")
	before := append([]byte(nil), source...)
	projection := InspectJSONWithSchema(source, func(pointer, key string) bool {
		return pointer == "" && (key == "model" || key == "a/b") || pointer == "/a~1b/1" && key == "x"
	})
	if !projection.Valid || projection.Status != ParseOK || !projection.Complete {
		t.Fatalf("unexpected parse result: valid=%v status=%s complete=%v warnings=%+v", projection.Valid, projection.Status, projection.Complete, projection.Warnings)
	}
	if !bytes.Equal(source, before) {
		t.Fatalf("InspectJSON mutated source: got %q want %q", source, before)
	}
	if projection.SourceHash != SourceHash(source) || !bytes.Equal(projection.Source, source) {
		t.Fatalf("projection did not retain source bytes")
	}
	if projection.Root == nil || projection.Root.Kind != JSONObject {
		t.Fatalf("root = %#v, want object", projection.Root)
	}
	if len(projection.Root.Fields) != 3 {
		t.Fatalf("fields = %d, want duplicate-preserving 3", len(projection.Root.Fields))
	}
	field, ok := projection.Root.Field("a/b")
	if !ok || field.Pointer != "/a~1b" || field.Value == nil || len(field.Value.Items) != 2 {
		t.Fatalf("a/b field = %#v", field)
	}
	if got := string(field.Value.Items[0].Raw); got != "1.00" {
		t.Fatalf("number raw = %q, want 1.00", got)
	}
	if got, ok := projection.Root.Lookup("/a~1b/1/x"); !ok || got.Kind != JSONBoolean || got.Value != true {
		t.Fatalf("pointer lookup = %#v, %v", got, ok)
	}
	if len(projection.UnknownNodes) != 1 || projection.UnknownNodes[0].Pointer != "/a~1b/1/unknown" {
		t.Fatalf("unknown nodes = %#v", projection.UnknownNodes)
	}
	// Raw/source are independent copies.  Mutating the projection must not
	// mutate the caller's artifact.
	projection.Source[0] = 'X'
	field.Value.Items[0].Raw[0] = '9'
	if !bytes.Equal(source, before) {
		t.Fatalf("projection aliases source bytes")
	}
}

func TestInspectJSONMalformedIsWarningAndNeverMutates(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"model":"x","tools":[1,],"vendor":{"nested":true}}`),
		[]byte(`{"message":"unterminated}`),
		[]byte(`{"n":01}`),
		[]byte(`{"ok":true} trailing`),
		[]byte{},
	}
	for _, source := range cases {
		before := append([]byte(nil), source...)
		projection := InspectJSON(source)
		if len(projection.Warnings) == 0 {
			t.Errorf("source %q produced no warnings", source)
		}
		if projection.Valid {
			t.Errorf("source %q marked valid despite warnings=%+v", source, projection.Warnings)
		}
		if !bytes.Equal(source, before) {
			t.Errorf("source %q mutated to %q", before, source)
		}
	}
}

func TestInspectSSERetainsExactEventsAndUnknownFields(t *testing.T) {
	source := []byte(": keep\r\nevent: response.output_text.delta\r\nid: abc\r\ndata: {\"delta\":\r\ndata: \"ab\"}\r\nx-provider: yes\r\n\r\nevent: done\ndata: [DONE]\n\n")
	before := append([]byte(nil), source...)
	projection := InspectSSE(source)
	if !projection.Valid || projection.Status != ParseOK || !projection.Complete {
		t.Fatalf("unexpected SSE result: valid=%v status=%s complete=%v warnings=%+v", projection.Valid, projection.Status, projection.Complete, projection.Warnings)
	}
	if !bytes.Equal(source, before) || !bytes.Equal(projection.Source, source) {
		t.Fatalf("SSE inspection changed source bytes")
	}
	if len(projection.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(projection.Events))
	}
	first := projection.Events[0]
	wantRaw := ": keep\r\nevent: response.output_text.delta\r\nid: abc\r\ndata: {\"delta\":\r\ndata: \"ab\"}\r\nx-provider: yes\r\n\r\n"
	if string(first.Raw) != wantRaw || first.Span.Start != 0 || first.Span.End != len(wantRaw) {
		t.Fatalf("first raw/span = %q/%+v", first.Raw, first.Span)
	}
	if first.Name != "response.output_text.delta" || first.ID != "abc" || first.Data != `{"delta":
"ab"}` {
		t.Fatalf("first event = %#v", first)
	}
	if len(first.Unknown) != 1 || first.Unknown[0].Name != "x-provider" {
		t.Fatalf("unknown fields = %#v", first.Unknown)
	}
	if first.JSON == nil || first.JSON.Status != ParseOK || first.JSON.Root == nil || first.JSON.Root.Kind != JSONObject {
		// Multi-line SSE data is joined by a newline and is not one JSON value;
		// retaining a warning/projection is expected, not a transport failure.
		t.Fatalf("joined event data JSON = %#v", first.JSON)
	}
	if len(projection.Events[1].Fields) != 2 || projection.Events[1].Data != "[DONE]" {
		t.Fatalf("second event = %#v", projection.Events[1])
	}
	if len(projection.Comments) != 1 || projection.Comments[0].Text != " keep" {
		t.Fatalf("comments = %#v", projection.Comments)
	}
	projection.Source[0] = 'X'
	projection.Events[0].Raw[0] = 'X'
	if !bytes.Equal(source, before) {
		t.Fatalf("SSE projection aliases source bytes")
	}
}

func TestInspectSSEMalformedRetryAndIncompleteEvent(t *testing.T) {
	source := []byte("retry: nope\ndata: {\"ok\":true}\n\nevent: final\ndata: {\"ok\":false}")
	before := append([]byte(nil), source...)
	projection := InspectSSE(source)
	if projection.Valid != true || projection.Status != ParsePartial || projection.Complete {
		t.Fatalf("unexpected malformed SSE result: valid=%v status=%s complete=%v", projection.Valid, projection.Status, projection.Complete)
	}
	if len(projection.Events) != 2 || projection.Events[1].Complete {
		t.Fatalf("events = %#v", projection.Events)
	}
	if len(projection.Warnings) < 2 {
		t.Fatalf("warnings = %#v", projection.Warnings)
	}
	if !bytes.Equal(source, before) {
		t.Fatalf("source mutated")
	}
}

func TestHintProtocolUsesPathHeadersAndBodyWithoutSecrets(t *testing.T) {
	cases := []struct {
		name     string
		input    HintInput
		protocol Protocol
		format   BodyFormat
		minConf  HintConfidence
	}{
		{name: "responses path", input: HintInput{Path: "/proxy/v1/responses?keep=raw", Body: []byte(`{"model":"m"}`)}, protocol: ProtocolResponses, format: FormatJSON, minConf: ConfidenceHigh},
		{name: "chat path", input: HintInput{Path: "/v1/chat/completions", ContentType: "application/json", Body: []byte(`{"messages":[]}`)}, protocol: ProtocolChatCompletions, format: FormatJSON, minConf: ConfidenceHigh},
		{name: "anthropic header", input: HintInput{Path: "/custom", Headers: http.Header{"Anthropic-Version": {"2023-06-01"}}, Body: []byte(`{"messages":[],"max_tokens":8}`)}, protocol: ProtocolAnthropicMessages, format: FormatJSON, minConf: ConfidenceHigh},
		{name: "chat done", input: HintInput{ContentType: "text/event-stream", Body: []byte("data: {\"object\":\"chat.completion.chunk\"}\n\ndata: [DONE]\n\n")}, protocol: ProtocolChatCompletions, format: FormatSSE, minConf: ConfidenceMedium},
		{name: "generic", input: HintInput{Body: []byte(`{"vendor_field":true}`)}, protocol: ProtocolGenericJSON, format: FormatJSON, minConf: ConfidenceLow},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			hint := HintProtocol(testCase.input)
			if hint.Protocol != testCase.protocol || hint.Format != testCase.format || confidenceRank(hint.Confidence) < confidenceRank(testCase.minConf) {
				t.Fatalf("hint = %#v", hint)
			}
		})
	}
	secretInput := HintInput{Path: "/v1/messages", Headers: http.Header{"X-Api-Key": {"do-not-display"}}, Body: []byte(`{"model":"m"}`)}
	hint := HintProtocol(secretInput)
	for _, evidence := range hint.Evidence {
		if strings.Contains(evidence, "do-not-display") {
			t.Fatalf("hint leaked secret in evidence: %q", evidence)
		}
	}
}
