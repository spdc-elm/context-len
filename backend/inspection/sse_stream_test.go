package inspection

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStreamScannerMatchesWholeBodyInspection(t *testing.T) {
	cases := []string{
		"responses/sse/response.sse",
		"chat_completions/sse/response.sse",
		"anthropic_messages/sse/response.sse",
	}
	for _, fixture := range cases {
		t.Run(filepath.Base(filepath.Dir(filepath.Dir(fixture))), func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", fixture))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			whole := InspectSSE(body)

			scanner := &StreamScanner{}
			var events []SSEEvent
			for i := 0; i < len(body); i++ {
				events = append(events, scanner.Write(body[i:i+1])...)
			}
			events = append(events, scanner.Flush()...)

			if len(events) != len(whole.Events) {
				t.Fatalf("byte-by-byte: got %d events, whole-body inspection got %d", len(events), len(whole.Events))
			}
			for i := range events {
				got, want := events[i], whole.Events[i]
				if got.Index != want.Index || got.Name != want.Name || got.ID != want.ID || got.Data != want.Data || got.Complete != want.Complete {
					t.Fatalf("event %d mismatch:\n scanner: %+v\n whole:   %+v", i, got, want)
				}
				if string(got.Raw) != string(want.Raw) {
					t.Fatalf("event %d raw mismatch:\n scanner: %q\n whole:   %q", i, got.Raw, want.Raw)
				}
				if got.Span != want.Span {
					t.Fatalf("event %d span mismatch: scanner %v whole %v", i, got.Span, want.Span)
				}
			}
		})
	}
}

func TestStreamScannerSplitsCRLFAcrossChunks(t *testing.T) {
	// "data: x\r\n\r\n" split so the CR and LF land in different chunks.
	scanner := &StreamScanner{}
	var events []SSEEvent
	for _, chunk := range []string{"data: x\r", "\n\r", "\ndata: y\r\n\r\n"} {
		events = append(events, scanner.Write([]byte(chunk))...)
	}
	events = append(events, scanner.Flush()...)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	if events[0].Data != "x" || events[1].Data != "y" {
		t.Fatalf("unexpected data: %q %q", events[0].Data, events[1].Data)
	}
}

func TestStreamScannerTruncatedRecord(t *testing.T) {
	scanner := &StreamScanner{}
	events := scanner.Write([]byte("event: partial\ndata: {\"trunc"))
	events = append(events, scanner.Flush()...)
	if len(events) != 1 {
		t.Fatalf("expected 1 incomplete event, got %d", len(events))
	}
	if events[0].Complete {
		t.Fatal("flushed event must be marked incomplete")
	}
	if events[0].Name != "partial" {
		t.Fatalf("unexpected name %q", events[0].Name)
	}
	if events[0].Data != `{"trunc` {
		t.Fatalf("unexpected data %q", events[0].Data)
	}
}

func TestStreamScannerMultiDataLinesAndDoneSentinel(t *testing.T) {
	body := "data: {\"a\":\ndata: 1}\n\ndata: [DONE]\n\n"
	scanner := &StreamScanner{}
	var events []SSEEvent
	events = append(events, scanner.Write([]byte(body[:9]))...)
	events = append(events, scanner.Write([]byte(body[9:]))...)
	events = append(events, scanner.Flush()...)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Data != "{\"a\":\n1}" {
		t.Fatalf("multi-line data mismatch: %q", events[0].Data)
	}
	if events[0].DataLines[0] != "{\"a\":" || events[0].DataLines[1] != "1}" {
		t.Fatalf("data lines not retained: %v", events[0].DataLines)
	}
	if events[1].Data != "[DONE]" {
		t.Fatalf("sentinel mismatch: %q", events[1].Data)
	}
}

func TestStreamScannerIgnoresCommentOnlyRecords(t *testing.T) {
	scanner := &StreamScanner{}
	events := scanner.Write([]byte(": ping\n: still ping\n\ndata: real\n\n"))
	events = append(events, scanner.Flush()...)
	if len(events) != 1 || events[0].Data != "real" {
		t.Fatalf("expected only the data record, got %+v", events)
	}
}

func TestStreamScannerEmptyWrite(t *testing.T) {
	scanner := &StreamScanner{}
	if events := scanner.Write(nil); len(events) != 0 {
		t.Fatalf("empty write emitted events: %+v", events)
	}
}
