package inspection

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// InspectSSE parses an event-stream body into a projection while retaining
// every event's original bytes.  It implements the field and line rules from
// the Server-Sent Events algorithm, but intentionally does not dispatch a
// transport stream or synthesize a [DONE] event.
func InspectSSE(source []byte) SSEInspection {
	owned := cloneBytes(source)
	result := SSEInspection{
		Format:     FormatSSE,
		Status:     ParseOK,
		Valid:      true,
		Complete:   true,
		Source:     owned,
		SourceHash: SourceHash(owned),
	}
	parser := sseParser{source: owned, result: &result}
	parser.parse()
	return finishSSE(result)
}

type sseParser struct {
	source []byte
	result *SSEInspection
	// Current event state.  A nil current event means only comments/empty lines
	// have been observed since the previous dispatch.
	current *sseEventBuilder
}

type sseEventBuilder struct {
	start     int
	fields    []SSEField
	unknown   []SSEField
	comments  []SSEComment
	name      string
	id        string
	retry     *int
	data      strings.Builder
	dataLines []string
	hasData   bool
	hasEvent  bool
	hasID     bool
	hasRetry  bool
}

func (p *sseParser) parse() {
	pos := 0
	for pos < len(p.source) {
		lineStart := pos
		lineEnd, next := nextSSELine(p.source, pos)
		lineRaw := cloneBytes(p.source[lineStart:next])
		line := p.source[lineStart:lineEnd]
		if lineEnd == lineStart {
			// A blank line dispatches the accumulated event.  Empty lines without
			// an event are intentionally ignored, as prescribed by SSE.
			if p.current != nil {
				p.dispatch(next, true)
			}
			pos = next
			continue
		}
		if p.current == nil {
			p.current = &sseEventBuilder{start: lineStart}
		}
		if line[0] == ':' {
			p.current.comments = append(p.current.comments, SSEComment{
				Text: string(line[1:]),
				Raw:  cloneBytes(lineRaw),
				Span: ByteSpan{lineStart, next},
			})
			pos = next
			continue
		}

		nameBytes := line
		valueBytes := []byte(nil)
		if colon := indexByte(line, ':'); colon >= 0 {
			nameBytes = line[:colon]
			valueBytes = line[colon+1:]
			if len(valueBytes) > 0 && valueBytes[0] == ' ' {
				valueBytes = valueBytes[1:]
			}
		}
		field := SSEField{Name: string(nameBytes), Value: string(valueBytes), Raw: lineRaw, Span: ByteSpan{lineStart, next}}
		p.current.applyField(field, p.result)
		if !utf8.Valid(line) {
			p.result.Warnings = append(p.result.Warnings, warning("invalid_utf8", "SSE line is not valid UTF-8; raw bytes are retained", "/events", field.Span, false))
		}
		pos = next
	}
	if p.current != nil {
		// SSE only dispatches an event at a blank line.  For observation we still
		// expose an incomplete final event, with an explicit warning and status.
		p.dispatch(len(p.source), false)
	}
}

// applyField folds one parsed SSE field line into the record under
// construction.  It is shared by the whole-buffer parser and the incremental
// StreamScanner so both observe identical field semantics.  Parser warnings
// are appended to result; the scanner passes a scratch value it discards
// because the complete-artifact inspection reports them once the body exists.
func (b *sseEventBuilder) applyField(field SSEField, result *SSEInspection) {
	b.fields = append(b.fields, field)
	switch field.Name {
	case "event":
		b.name = field.Value
		b.hasEvent = true
	case "data":
		b.data.WriteString(field.Value)
		b.data.WriteByte('\n')
		b.dataLines = append(b.dataLines, field.Value)
		b.hasData = true
	case "id":
		// The SSE algorithm ignores id values containing NUL.  Retaining the
		// field and warning is more useful for inspection than silently
		// losing its raw bytes.
		if strings.IndexByte(field.Value, 0) >= 0 {
			result.Warnings = append(result.Warnings, warning("nul_in_id", "SSE id field contains NUL and is not a valid event id", "/events", field.Span, false))
		} else {
			b.id = field.Value
			b.hasID = true
		}
	case "retry":
		b.hasRetry = true
		n, err := parseRetry(field.Value)
		if err != nil {
			b.unknown = append(b.unknown, field)
			result.Warnings = append(result.Warnings, warning("invalid_retry", "SSE retry field must be a non-negative decimal integer", "/events", field.Span, false))
		} else {
			b.retry = &n
		}
	default:
		// Unknown SSE field names have no forwarding effect, but retaining
		// them is important when explaining a provider extension.
		b.unknown = append(b.unknown, field)
	}
}

func (p *sseParser) dispatch(end int, complete bool) {
	if p.current == nil {
		return
	}
	b := p.current
	comments := cloneSSEComments(b.comments)
	p.result.Comments = append(p.result.Comments, comments...)
	// Per the SSE dispatch algorithm, a record with no data field does not
	// produce an event.  Comments, id, retry, event, and unknown extension
	// fields still remain observable below, but must not be mistaken for a
	// client-visible event.
	if !b.hasData {
		for i, field := range b.unknown {
			p.result.UnknownNodes = append(p.result.UnknownNodes, UnknownNode{
				Pointer: "/orphan_fields/" + strconv.Itoa(len(p.result.UnknownNodes)+i),
				Kind:    "sse_field",
				Raw:     cloneBytes(field.Raw),
				Span:    field.Span,
				Reason:  "unrecognised SSE field in a record without data",
			})
		}
		if !complete {
			p.result.Complete = false
			p.result.Warnings = append(p.result.Warnings, warning("incomplete_record", "SSE stream ended before a data event delimiter", "/orphan_fields", ByteSpan{b.start, end}, false))
		}
		p.current = nil
		return
	}
	data := b.data.String()
	if b.hasData && strings.HasSuffix(data, "\n") {
		data = strings.TrimSuffix(data, "\n")
	}
	event := SSEEvent{
		Index:     len(p.result.Events),
		Name:      b.name,
		ID:        b.id,
		Retry:     cloneIntPtr(b.retry),
		Data:      data,
		DataLines: append([]string(nil), b.dataLines...),
		Fields:    cloneSSEFields(b.fields),
		Unknown:   cloneSSEFields(b.unknown),
		Raw:       cloneBytes(p.source[b.start:end]),
		Span:      ByteSpan{b.start, end},
		Complete:  complete,
	}
	if b.hasData {
		// Event data is a projection in its own right.  JSON parse failures are
		// warnings on the event, never a reason to discard its raw bytes.
		if strings.TrimSpace(data) != "" && data != "[DONE]" {
			jsonProjection := InspectJSON([]byte(data))
			event.JSON = &jsonProjection
			for _, item := range jsonProjection.Warnings {
				item.Pointer = "/events/" + strconv.Itoa(event.Index) + "/data" + item.Pointer
				item.Fatal = false // malformed event data does not invalidate SSE
				p.result.Warnings = append(p.result.Warnings, item)
			}
		}
	}
	p.result.Events = append(p.result.Events, event)
	if len(b.unknown) > 0 {
		for i, field := range b.unknown {
			p.result.UnknownNodes = append(p.result.UnknownNodes, UnknownNode{
				Pointer: "/events/" + strconv.Itoa(event.Index) + "/fields/" + strconv.Itoa(i),
				Kind:    "sse_field",
				Raw:     cloneBytes(field.Raw),
				Span:    field.Span,
				Reason:  "unrecognised SSE field",
			})
		}
	}
	if !complete {
		p.result.Complete = false
		p.result.Warnings = append(p.result.Warnings, warning("incomplete_event", "SSE stream ended before an event delimiter", "/events/"+strconv.Itoa(event.Index), event.Span, false))
	}
	p.current = nil
}

func finishSSE(result SSEInspection) SSEInspection {
	fatal := false
	for _, item := range result.Warnings {
		if item.Fatal {
			fatal = true
			break
		}
	}
	if fatal {
		result.Valid = false
		result.Status = ParseInvalid
	} else if len(result.Warnings) > 0 {
		result.Status = ParsePartial
	}
	return result
}

// nextSSELine returns the byte offset immediately before a line terminator and
// the offset after it.  CRLF is one terminator; lone CR and lone LF are also
// accepted, matching the SSE line parser.
func nextSSELine(source []byte, start int) (lineEnd, next int) {
	for i := start; i < len(source); i++ {
		switch source[i] {
		case '\n':
			return i, i + 1
		case '\r':
			if i+1 < len(source) && source[i+1] == '\n' {
				return i, i + 2
			}
			return i, i + 1
		}
	}
	return len(source), len(source)
}

func parseRetry(value string) (int, error) {
	if value == "" {
		return 0, strconv.ErrSyntax
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, strconv.ErrSyntax
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n > int64(maxInt()) {
		return 0, strconv.ErrRange
	}
	return int(n), nil
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

func cloneIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneSSEFields(fields []SSEField) []SSEField {
	if len(fields) == 0 {
		return nil
	}
	result := make([]SSEField, len(fields))
	for i := range fields {
		result[i] = fields[i]
		result[i].Raw = cloneBytes(fields[i].Raw)
	}
	return result
}

func cloneSSEComments(comments []SSEComment) []SSEComment {
	if len(comments) == 0 {
		return nil
	}
	result := make([]SSEComment, len(comments))
	for i := range comments {
		result[i] = comments[i]
		result[i].Raw = cloneBytes(comments[i].Raw)
	}
	return result
}

func indexByte(value []byte, target byte) int {
	for i, current := range value {
		if current == target {
			return i
		}
	}
	return -1
}
