package inspection

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// JSONSchema reports whether an object key is known at a JSON pointer.  A nil
// schema means generic JSON inspection: every field is retained, but no field
// is classified as unknown.  Protocol-specific inspectors can pass a schema
// while still using this loss-aware parser.
type JSONSchema func(pointer, key string) bool

// InspectJSON parses source into a generic, loss-aware projection.  It never
// modifies source and never marshals a parsed map back to bytes.
func InspectJSON(source []byte) JSONInspection {
	return inspectJSON(source, nil)
}

// InspectJSONWithSchema is the protocol-inspector hook for classifying unknown
// object fields.  Unknown fields remain in Root.Fields and UnknownNodes with
// their original raw fragment and byte span.
func InspectJSONWithSchema(source []byte, schema JSONSchema) JSONInspection {
	return inspectJSON(source, schema)
}

func inspectJSON(source []byte, schema JSONSchema) JSONInspection {
	owned := cloneBytes(source)
	result := JSONInspection{
		Format:     FormatJSON,
		Status:     ParseOK,
		Valid:      true,
		Complete:   true,
		Source:     owned,
		SourceHash: SourceHash(owned),
	}
	p := jsonParser{source: owned, schema: schema, result: &result}
	p.skipSpace()
	if p.atEnd() {
		p.addWarning("empty_json", "JSON body is empty", "", ByteSpan{0, 0}, true)
		result.Root = &JSONNode{Kind: JSONInvalid, Pointer: "", Span: ByteSpan{0, 0}, Raw: nil}
		return finishJSON(result)
	}
	root := p.parseValue("")
	if root == nil {
		root = &JSONNode{Kind: JSONInvalid, Pointer: "", Span: ByteSpan{p.pos, p.pos}, Raw: nil}
	}
	result.Root = root
	p.skipSpace()
	if !p.atEnd() {
		start := p.pos
		p.addWarning("trailing_json", "unexpected bytes after the JSON value", "", ByteSpan{start, len(owned)}, true)
	}
	return finishJSON(result)
}

func finishJSON(result JSONInspection) JSONInspection {
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

type jsonParser struct {
	source []byte
	pos    int
	schema JSONSchema
	result *JSONInspection
}

func (p *jsonParser) atEnd() bool { return p.pos >= len(p.source) }

func (p *jsonParser) skipSpace() {
	for !p.atEnd() {
		switch p.source[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonParser) addWarning(code, message, pointer string, span ByteSpan, fatal bool) {
	p.result.Warnings = append(p.result.Warnings, warning(code, message, pointer, span, fatal))
}

func (p *jsonParser) parseValue(pointer string) *JSONNode {
	p.skipSpace()
	start := p.pos
	if p.atEnd() {
		p.addWarning("unexpected_eof", "expected a JSON value", pointer, ByteSpan{start, start}, true)
		return nil
	}
	var node *JSONNode
	switch p.source[p.pos] {
	case '{':
		node = p.parseObject(pointer)
	case '[':
		node = p.parseArray(pointer)
	case '"':
		node = p.parseString(pointer)
	case 't':
		node = p.parseLiteral(pointer, "true", JSONBoolean, true)
	case 'f':
		node = p.parseLiteral(pointer, "false", JSONBoolean, false)
	case 'n':
		node = p.parseLiteral(pointer, "null", JSONNull, nil)
	default:
		if p.source[p.pos] == '-' || (p.source[p.pos] >= '0' && p.source[p.pos] <= '9') {
			node = p.parseNumber(pointer)
		} else {
			p.pos++
			p.addWarning("invalid_value", fmt.Sprintf("unexpected byte %q while reading JSON value", p.source[start]), pointer, ByteSpan{start, p.pos}, true)
			return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos])}
		}
	}
	return node
}

func (p *jsonParser) parseObject(pointer string) *JSONNode {
	start := p.pos
	p.pos++ // {
	node := &JSONNode{Kind: JSONObject, Pointer: pointer, Span: ByteSpan{Start: start}, Raw: nil}
	p.skipSpace()
	if p.consume('}') {
		node.Span.End = p.pos
		node.Raw = cloneBytes(p.source[start:p.pos])
		return node
	}
	for {
		p.skipSpace()
		keyStart := p.pos
		if p.atEnd() {
			p.addWarning("unexpected_eof", "unterminated JSON object; expected a key", pointer, ByteSpan{p.pos, p.pos}, true)
			node.Span.End = p.pos
			node.Raw = cloneBytes(p.source[start:p.pos])
			return node
		}
		if p.source[p.pos] != '"' {
			badStart := p.pos
			p.consumeUntilObjectBoundary()
			p.addWarning("object_key_expected", "expected a quoted object key", pointer, ByteSpan{badStart, p.pos}, true)
			if p.consume('}') {
				node.Span.End = p.pos
				node.Raw = cloneBytes(p.source[start:p.pos])
				return node
			}
			if p.consume(',') {
				continue
			}
			node.Span.End = p.pos
			node.Raw = cloneBytes(p.source[start:p.pos])
			return node
		}
		key, keyRaw, ok := p.parseKey()
		if !ok {
			p.addWarning("invalid_object_key", "invalid JSON object key", pointer, ByteSpan{keyStart, p.pos}, true)
			p.consumeUntilObjectBoundary()
			if p.consume('}') {
				node.Span.End = p.pos
				node.Raw = cloneBytes(p.source[start:p.pos])
				return node
			}
			if p.consume(',') {
				continue
			}
			node.Span.End = p.pos
			node.Raw = cloneBytes(p.source[start:p.pos])
			return node
		}
		p.skipSpace()
		if !p.consume(':') {
			p.addWarning("object_colon_expected", "expected ':' after object key", pointerJoin(pointer, key), ByteSpan{p.pos, minInt(p.pos+1, len(p.source))}, true)
			// Try to retain a value if one starts here; otherwise recover at a
			// comma/closing brace.
			if p.atEnd() {
				node.Span.End = p.pos
				node.Raw = cloneBytes(p.source[start:p.pos])
				return node
			}
		}
		childPointer := pointerJoin(pointer, key)
		value := p.parseValue(childPointer)
		if value == nil {
			p.consumeUntilObjectBoundary()
		}
		fieldEnd := p.pos
		field := JSONField{
			Key:     key,
			RawKey:  cloneBytes(keyRaw),
			Pointer: childPointer,
			Span:    ByteSpan{Start: keyStart, End: fieldEnd},
			Raw:     cloneBytes(p.source[keyStart:fieldEnd]),
			Value:   value,
		}
		if p.schema != nil && !p.schema(pointer, key) {
			field.Unknown = true
			p.result.UnknownNodes = append(p.result.UnknownNodes, UnknownNode{
				Pointer: childPointer,
				Kind:    kindOf(value),
				Raw:     cloneBytes(p.source[keyStart:fieldEnd]),
				Span:    field.Span,
				Reason:  "unrecognised object field",
			})
		}
		node.Fields = append(node.Fields, field)
		p.skipSpace()
		if p.consume('}') {
			node.Span.End = p.pos
			node.Raw = cloneBytes(p.source[start:p.pos])
			return node
		}
		if p.consume(',') {
			p.skipSpace()
			if p.consume('}') {
				p.addWarning("trailing_object_comma", "trailing comma in JSON object", pointer, ByteSpan{p.pos - 1, p.pos}, true)
				node.Span.End = p.pos
				node.Raw = cloneBytes(p.source[start:p.pos])
				return node
			}
			continue
		}
		if p.atEnd() {
			p.addWarning("unexpected_eof", "unterminated JSON object; expected ',' or '}'", pointer, ByteSpan{p.pos, p.pos}, true)
			node.Span.End = p.pos
			node.Raw = cloneBytes(p.source[start:p.pos])
			return node
		}
		badStart := p.pos
		p.consumeUntilObjectBoundary()
		p.addWarning("object_separator_expected", "expected ',' or '}' after object member", pointer, ByteSpan{badStart, p.pos}, true)
		if p.consume('}') {
			node.Span.End = p.pos
			node.Raw = cloneBytes(p.source[start:p.pos])
			return node
		}
		if p.atEnd() {
			node.Span.End = p.pos
			node.Raw = cloneBytes(p.source[start:p.pos])
			return node
		}
	}
}

func (p *jsonParser) parseArray(pointer string) *JSONNode {
	start := p.pos
	p.pos++ // [
	node := &JSONNode{Kind: JSONArray, Pointer: pointer, Span: ByteSpan{Start: start}}
	p.skipSpace()
	if p.consume(']') {
		node.Span.End = p.pos
		node.Raw = cloneBytes(p.source[start:p.pos])
		return node
	}
	index := 0
	for {
		p.skipSpace()
		itemPointer := pointerJoin(pointer, strconv.Itoa(index))
		before := p.pos
		item := p.parseValue(itemPointer)
		if item == nil {
			p.consumeUntilArrayBoundary()
		}
		if p.pos == before && !p.atEnd() {
			p.pos++
		}
		node.Items = append(node.Items, item)
		index++
		p.skipSpace()
		if p.consume(']') {
			node.Span.End = p.pos
			node.Raw = cloneBytes(p.source[start:p.pos])
			return node
		}
		if p.consume(',') {
			p.skipSpace()
			if p.consume(']') {
				p.addWarning("trailing_array_comma", "trailing comma in JSON array", pointer, ByteSpan{p.pos - 1, p.pos}, true)
				node.Span.End = p.pos
				node.Raw = cloneBytes(p.source[start:p.pos])
				return node
			}
			continue
		}
		if p.atEnd() {
			p.addWarning("unexpected_eof", "unterminated JSON array; expected ',' or ']'", pointer, ByteSpan{p.pos, p.pos}, true)
			node.Span.End = p.pos
			node.Raw = cloneBytes(p.source[start:p.pos])
			return node
		}
		badStart := p.pos
		p.consumeUntilArrayBoundary()
		p.addWarning("array_separator_expected", "expected ',' or ']' after array item", pointer, ByteSpan{badStart, p.pos}, true)
		if p.consume(']') {
			node.Span.End = p.pos
			node.Raw = cloneBytes(p.source[start:p.pos])
			return node
		}
		if p.atEnd() {
			node.Span.End = p.pos
			node.Raw = cloneBytes(p.source[start:p.pos])
			return node
		}
	}
}

func (p *jsonParser) parseKey() (string, []byte, bool) {
	start := p.pos
	value := p.parseString("")
	if value == nil || value.Kind != JSONString {
		return "", p.source[start:p.pos], false
	}
	key, ok := value.Value.(string)
	return key, p.source[start:p.pos], ok
}

func (p *jsonParser) parseString(pointer string) *JSONNode {
	start := p.pos
	if !p.consume('"') {
		return nil
	}
	for !p.atEnd() {
		c := p.source[p.pos]
		switch c {
		case '"':
			p.pos++
			raw := cloneBytes(p.source[start:p.pos])
			var decoded string
			if err := json.Unmarshal(raw, &decoded); err != nil {
				p.addWarning("invalid_string", "invalid JSON string escape", pointer, ByteSpan{start, p.pos}, true)
				return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: raw}
			}
			return &JSONNode{Kind: JSONString, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: raw, Value: decoded}
		case '\\':
			p.pos++
			if p.atEnd() {
				p.addWarning("unexpected_eof", "unterminated escape in JSON string", pointer, ByteSpan{start, p.pos}, true)
				return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos])}
			}
			if p.source[p.pos] == 'u' {
				p.pos++
				for i := 0; i < 4; i++ {
					if p.atEnd() || !isHex(p.source[p.pos]) {
						if !p.atEnd() {
							p.pos++
						}
						p.addWarning("invalid_string_escape", "invalid unicode escape in JSON string", pointer, ByteSpan{start, p.pos}, true)
						return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos])}
					}
					p.pos++
				}
				continue
			}
			if !strings.ContainsRune(`"\\/bfnrt`, rune(p.source[p.pos])) {
				p.pos++
				p.addWarning("invalid_string_escape", "invalid escape in JSON string", pointer, ByteSpan{start, p.pos}, true)
				return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos])}
			}
			p.pos++
		default:
			if c < 0x20 {
				p.pos++
				p.addWarning("control_in_string", "unescaped control byte in JSON string", pointer, ByteSpan{start, p.pos}, true)
				return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos])}
			}
			p.pos++
		}
	}
	p.addWarning("unexpected_eof", "unterminated JSON string", pointer, ByteSpan{start, p.pos}, true)
	return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos])}
}

func (p *jsonParser) parseLiteral(pointer, literal string, kind JSONKind, value any) *JSONNode {
	start := p.pos
	if len(p.source)-p.pos < len(literal) || string(p.source[p.pos:p.pos+len(literal)]) != literal {
		end := minInt(len(p.source), p.pos+len(literal))
		p.pos = end
		p.addWarning("invalid_literal", "invalid JSON literal", pointer, ByteSpan{start, end}, true)
		return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, end}, Raw: cloneBytes(p.source[start:end])}
	}
	p.pos += len(literal)
	// A literal must not be immediately followed by a value character.  The
	// enclosing parser will report separators/trailing bytes otherwise, but a
	// dedicated warning makes the projection more useful.
	if !p.atEnd() && !isJSONDelimiter(p.source[p.pos]) {
		for !p.atEnd() && !isJSONDelimiter(p.source[p.pos]) {
			p.pos++
		}
		p.addWarning("invalid_literal_boundary", "JSON literal is followed by an invalid byte", pointer, ByteSpan{start, p.pos}, true)
		return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos])}
	}
	return &JSONNode{Kind: kind, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos]), Value: value}
}

func (p *jsonParser) parseNumber(pointer string) *JSONNode {
	start := p.pos
	if p.consume('-') && p.atEnd() {
		p.addWarning("invalid_number", "JSON number is missing digits", pointer, ByteSpan{start, p.pos}, true)
		return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos])}
	}
	if !p.atEnd() && p.source[p.pos] == '0' {
		p.pos++
		if !p.atEnd() && p.source[p.pos] >= '0' && p.source[p.pos] <= '9' {
			p.addWarning("invalid_number", "JSON number cannot contain a leading zero", pointer, ByteSpan{start, p.pos + 1}, true)
			for !p.atEnd() && isNumberContinuation(p.source[p.pos]) {
				p.pos++
			}
			return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos])}
		}
	} else {
		if p.atEnd() || p.source[p.pos] < '1' || p.source[p.pos] > '9' {
			end := minInt(len(p.source), p.pos+1)
			p.pos = end
			p.addWarning("invalid_number", "JSON number is missing integer digits", pointer, ByteSpan{start, end}, true)
			return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos])}
		}
		for !p.atEnd() && p.source[p.pos] >= '0' && p.source[p.pos] <= '9' {
			p.pos++
		}
	}
	if !p.atEnd() && p.source[p.pos] == '.' {
		p.pos++
		fractionStart := p.pos
		for !p.atEnd() && p.source[p.pos] >= '0' && p.source[p.pos] <= '9' {
			p.pos++
		}
		if p.pos == fractionStart {
			p.addWarning("invalid_number", "JSON fraction is missing digits", pointer, ByteSpan{start, p.pos}, true)
			return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos])}
		}
	}
	if !p.atEnd() && (p.source[p.pos] == 'e' || p.source[p.pos] == 'E') {
		p.pos++
		if !p.atEnd() && (p.source[p.pos] == '+' || p.source[p.pos] == '-') {
			p.pos++
		}
		exponentStart := p.pos
		for !p.atEnd() && p.source[p.pos] >= '0' && p.source[p.pos] <= '9' {
			p.pos++
		}
		if p.pos == exponentStart {
			p.addWarning("invalid_number", "JSON exponent is missing digits", pointer, ByteSpan{start, p.pos}, true)
			return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos])}
		}
	}
	if !p.atEnd() && !isJSONDelimiter(p.source[p.pos]) {
		for !p.atEnd() && isNumberContinuation(p.source[p.pos]) {
			p.pos++
		}
		p.addWarning("invalid_number_boundary", "JSON number is followed by an invalid byte", pointer, ByteSpan{start, p.pos}, true)
		return &JSONNode{Kind: JSONInvalid, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: cloneBytes(p.source[start:p.pos])}
	}
	raw := cloneBytes(p.source[start:p.pos])
	return &JSONNode{Kind: JSONNumber, Pointer: pointer, Span: ByteSpan{start, p.pos}, Raw: raw, Value: json.Number(string(raw))}
}

func (p *jsonParser) consume(expected byte) bool {
	if !p.atEnd() && p.source[p.pos] == expected {
		p.pos++
		return true
	}
	return false
}

func (p *jsonParser) consumeUntilObjectBoundary() {
	for !p.atEnd() && p.source[p.pos] != ',' && p.source[p.pos] != '}' {
		p.pos++
	}
}

func (p *jsonParser) consumeUntilArrayBoundary() {
	for !p.atEnd() && p.source[p.pos] != ',' && p.source[p.pos] != ']' {
		p.pos++
	}
}

func pointerJoin(parent, token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	token = strings.ReplaceAll(token, "/", "~1")
	if parent == "" {
		return "/" + token
	}
	return parent + "/" + token
}

func kindOf(node *JSONNode) string {
	if node == nil {
		return string(JSONInvalid)
	}
	return string(node.Kind)
}

func isJSONDelimiter(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ',' || c == ']' || c == '}'
}

func isNumberContinuation(c byte) bool {
	return !isJSONDelimiter(c)
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
