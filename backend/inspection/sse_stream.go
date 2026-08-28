package inspection

import (
	"bytes"
	"strings"
)

// StreamScanner incrementally parses an SSE body while it is still being
// read.  It is the streaming counterpart of InspectSSE: the same line and
// field rules produce the same SSEEvent values, but records are emitted as
// soon as their terminating blank line arrives instead of after the whole
// body exists.  The scanner observes copies only; it never rewrites,
// aggregates, or re-encodes transport bytes.
//
// A well-formed stream ends with a blank line, so Flush normally emits
// nothing.  A stream truncated mid-record is dispatched by Flush with
// Complete=false, mirroring InspectSSE's incomplete final event.
type StreamScanner struct {
	pending []byte // unconsumed bytes; may end with a partial line
	offset  int64  // absolute stream offset of pending[0]

	builder    *sseEventBuilder // record accumulated so far, if any
	recordRaw  bytes.Buffer     // raw bytes of the record under construction
	recordAt   int64            // absolute offset where the record started
	scratch    SSEInspection     // warning sink; discarded (full inspection owns warnings)
	ordinal    int
}

// Write consumes one chunk of stream bytes and returns every SSE record that
// became complete.  Chunks may split lines, and even a CRLF pair, at any
// boundary; a trailing lone CR is held back until more bytes arrive or Flush
// resolves it.
func (s *StreamScanner) Write(chunk []byte) []SSEEvent {
	s.pending = append(s.pending, chunk...)
	return s.drain(false)
}

// Flush resolves any unterminated trailing line and dispatches a remaining
// incomplete record.  It must be called at most once, after the final chunk.
func (s *StreamScanner) Flush() []SSEEvent {
	var out []SSEEvent
	if len(s.pending) > 0 {
		// No terminator followed these bytes; process them as the final
		// unterminated line.
		line := s.pending
		out = append(out, s.processLine(line, line)...)
		s.offset += int64(len(line))
		s.pending = nil
	}
	out = append(out, s.dispatch(false, s.offset)...)
	return out
}

// drain processes every complete line in pending.  When final is false a
// trailing lone CR is retained because it may turn out to be half of a CRLF
// pair arriving in the next chunk.
func (s *StreamScanner) drain(final bool) []SSEEvent {
	var out []SSEEvent
	for len(s.pending) > 0 {
		lineEnd, next := nextSSELine(s.pending, 0)
		if lineEnd == next {
			// No line terminator in pending; wait for more bytes (Flush
			// consumes the remainder at end of stream).
			return out
		}
		if !final && next == len(s.pending) && s.pending[lineEnd] == '\r' {
			return out // lone CR might be the first half of CRLF
		}
		out = append(out, s.processLine(s.pending[:next], s.pending[:lineEnd])...)
		s.pending = s.pending[next:]
		s.offset += int64(next)
	}
	return out
}

// processLine folds one line into the scanner state.  line excludes its
// terminator while rawLine includes it (rawLine equals line for the
// unterminated final line).
func (s *StreamScanner) processLine(rawLine []byte, line []byte) []SSEEvent {
	s.recordRaw.Write(rawLine)
	if len(line) == 0 {
		// A blank line dispatches the accumulated record; its raw bytes close
		// the record span, matching InspectSSE's Raw/Span semantics.
		return s.dispatch(true, s.offset+int64(len(rawLine)))
	}
	if line[0] == ':' {
		// Comments belong to the record region but are only retained by the
		// whole-body inspection; nothing to accumulate here.
		s.ensureBuilder()
		return nil
	}
	s.ensureBuilder()
	nameBytes := line
	valueBytes := []byte(nil)
	if colon := indexByte(line, ':'); colon >= 0 {
		nameBytes = line[:colon]
		valueBytes = line[colon+1:]
		if len(valueBytes) > 0 && valueBytes[0] == ' ' {
			valueBytes = valueBytes[1:]
		}
	}
	field := SSEField{Name: string(nameBytes), Value: string(valueBytes), Raw: append([]byte(nil), rawLine...), Span: ByteSpan{Start: int(s.offset), End: int(s.offset) + len(rawLine)}}
	s.builder.applyField(field, &s.scratch)
	return nil
}

func (s *StreamScanner) ensureBuilder() {
	if s.builder == nil {
		s.builder = &sseEventBuilder{start: int(s.offset)}
		s.recordAt = s.offset
	}
}

// dispatch emits the accumulated record, if it is client-visible (has data).
// complete reports whether the terminating blank line was present; end is the
// absolute offset after the record's final raw bytes.
func (s *StreamScanner) dispatch(complete bool, end int64) []SSEEvent {
	b := s.builder
	s.builder = nil
	raw := append([]byte(nil), s.recordRaw.Bytes()...)
	s.recordRaw.Reset()
	if b == nil || !b.hasData {
		return nil
	}
	data := b.data.String()
	if strings.HasSuffix(data, "\n") {
		data = strings.TrimSuffix(data, "\n")
	}
	event := SSEEvent{
		Index:     s.ordinal,
		Name:      b.name,
		ID:        b.id,
		Retry:     cloneIntPtr(b.retry),
		Data:      data,
		DataLines: append([]string(nil), b.dataLines...),
		Fields:    cloneSSEFields(b.fields),
		Unknown:   cloneSSEFields(b.unknown),
		Raw:       raw,
		Span:      ByteSpan{Start: int(s.recordAt), End: int(end)},
		Complete:  complete,
	}
	s.ordinal++
	return []SSEEvent{event}
}
