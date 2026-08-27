// Package mutation implements explicit, auditable derived artifacts. It is never used by bypass transport.
package mutation

import (
	"bytes"
	"context-lens/backend/inspection"
	"context-lens/backend/wire"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

// Operation is the intentionally small JSON-Patch subset accepted by the MVP.
// add, replace, and remove are supported. Value is kept as any so unknown and
// provider extension values survive an explicit edit.
type Operation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// Result always points at a new derived artifact. Artifact is never the base
// artifact, even when a replacement happens to contain identical bytes. The
// original artifact remains immutable and BaseSHA256 binds the edit to the
// operator's observed revision.
type Result struct {
	Artifact   wire.BodyArtifact           `json:"artifact"`
	Diff       Diff                        `json:"diff"`
	BaseSHA256 string                      `json:"base_sha256"`
	Validated  bool                        `json:"validated"`
	Validation inspection.ValidationResult `json:"validation"`
	Protocol   inspection.Protocol         `json:"protocol,omitempty"`
	Format     inspection.BodyFormat       `json:"format,omitempty"`
}

type Diff struct {
	Changed bool    `json:"changed"`
	Entries []Entry `json:"entries"`
}

type Entry struct {
	Path   string `json:"path"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
	Kind   string `json:"kind"`
}

// Replace creates a derived raw artifact without interpreting body bytes. It
// is the generic/raw-replacement primitive retained for callers that do not
// know a protocol. Use ReplaceProtocol when release-time validation is needed.
func Replace(base wire.BodyArtifact, body []byte) (Result, error) {
	if len(body) == 0 {
		return Result{}, fmt.Errorf("mutation: replacement body is empty")
	}
	return derive(base, body, Diff{
		Changed: !bytes.Equal(base.Bytes(), body),
		Entries: replacementDiff(base.Bytes(), body),
	}, inspection.ProtocolUnknown, inspection.FormatUnknown, true), nil
}

// ReplaceProtocol creates a derived artifact and validates it against the
// selected provider protocol. A validation failure is represented in Result
// (Validated=false) rather than discarding the candidate, so an editor can
// display its diff and diagnostics before deciding whether to retry. Parsing
// and artifact construction errors still return a non-nil error.
func ReplaceProtocol(base wire.BodyArtifact, body []byte, protocol inspection.Protocol, format ...inspection.BodyFormat) (Result, error) {
	if len(body) == 0 {
		return Result{}, fmt.Errorf("mutation: replacement body is empty")
	}
	validation := inspection.ValidateProtocol(protocol, body, format...)
	return derive(base, body, Diff{
		Changed: !bytes.Equal(base.Bytes(), body),
		Entries: replacementDiff(base.Bytes(), body),
	}, protocol, validation.Format, validation.Valid).withValidation(validation), nil
}

// RawReplacement is a readable alias for ReplaceProtocol.
func RawReplacement(base wire.BodyArtifact, body []byte, protocol inspection.Protocol, format ...inspection.BodyFormat) (Result, error) {
	return ReplaceProtocol(base, body, protocol, format...)
}

// JSONPatch applies explicit JSON pointer operations and creates a derived
// artifact. It retains the historical generic API; use JSONPatchProtocol for
// protocol-aware validation. JSON is decoded only because the caller asked for
// an explicit mutation, never on a bypass path.
func JSONPatch(base wire.BodyArtifact, ops []Operation) (Result, error) {
	body, diff, err := applyJSONPatch(base.Bytes(), ops)
	if err != nil {
		return Result{}, err
	}
	return derive(base, body, diff, inspection.ProtocolUnknown, inspection.FormatJSON, true), nil
}

// JSONPatchProtocol applies explicit operations, computes a field-level diff,
// and validates the derived body using the protocol's JSON contract. Unknown
// fields remain in the patched tree and are reported by the projection.
func JSONPatchProtocol(base wire.BodyArtifact, ops []Operation, protocol inspection.Protocol, format ...inspection.BodyFormat) (Result, error) {
	body, diff, err := applyJSONPatch(base.Bytes(), ops)
	if err != nil {
		return Result{}, err
	}
	validation := inspection.ValidateProtocol(protocol, body, append([]inspection.BodyFormat{inspection.FormatJSON}, format...)...)
	return derive(base, body, diff, protocol, validation.Format, validation.Valid).withValidation(validation), nil
}

// ApplyJSONPatch is an alias useful to command handlers.
func ApplyJSONPatch(base wire.BodyArtifact, ops []Operation, protocol inspection.Protocol, format ...inspection.BodyFormat) (Result, error) {
	return JSONPatchProtocol(base, ops, protocol, format...)
}

// RequireValid rejects a candidate after its diff and diagnostics have been
// produced. It is useful at a release boundary that must not forward an
// invalid explicit mutation.
func RequireValid(result Result) (Result, error) {
	if !result.Validated {
		return result, fmt.Errorf("mutation: protocol validation failed: %s", strings.Join(result.Validation.ErrorMessages(), "; "))
	}
	return result, nil
}

func (r Result) withValidation(validation inspection.ValidationResult) Result {
	r.Validation = validation
	r.Validated = validation.Valid
	if r.Format == inspection.FormatUnknown {
		r.Format = validation.Format
	}
	return r
}

func derive(base wire.BodyArtifact, body []byte, diff Diff, protocol inspection.Protocol, format inspection.BodyFormat, validated bool) Result {
	a := wire.NewArtifact(body, wire.ArtifactOptions{
		Stage:           "derived",
		Direction:       base.Ref().Direction,
		ContentType:     base.Ref().ContentType,
		ContentEncoding: base.Ref().ContentEncoding,
	})
	return Result{
		Artifact:   a,
		Diff:       diff,
		BaseSHA256: base.Ref().SHA256,
		Validated:  validated,
		Protocol:   protocol,
		Format:     format,
	}
}

func replacementDiff(before, after []byte) []Entry {
	if bytes.Equal(before, after) {
		return nil
	}
	return []Entry{{Path: "", Before: map[string]any{"size": len(before), "sha256": wire.SHA256Hex(before)}, After: map[string]any{"size": len(after), "sha256": wire.SHA256Hex(after)}, Kind: "replace"}}
}

func applyJSONPatch(source []byte, ops []Operation) ([]byte, Diff, error) {
	var root any
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, Diff{}, fmt.Errorf("mutation: base JSON: %w", err)
	}
	// Reject non-whitespace trailing values. This avoids silently mutating a
	// prefix while the original body is malformed.
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, Diff{}, fmt.Errorf("mutation: base JSON has trailing value")
	} else if err != io.EOF {
		return nil, Diff{}, fmt.Errorf("mutation: base JSON has malformed trailing bytes: %w", err)
	}
	before := deepCloneJSON(root)
	for _, op := range ops {
		if err := applyOperation(&root, op); err != nil {
			return nil, Diff{}, err
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, Diff{}, fmt.Errorf("mutation: encode JSON: %w", err)
	}
	diff := jsonDiff(before, root)
	if !diff.Changed && !bytes.Equal(source, out) {
		// Formatting-only explicit patches still produce an auditable root diff.
		diff = Diff{Changed: true, Entries: replacementDiff(source, out)}
	}
	return out, diff, nil
}

func applyOperation(root *any, operation Operation) error {
	op := strings.ToLower(strings.TrimSpace(operation.Op))
	switch op {
	case "add", "replace", "remove":
	default:
		return fmt.Errorf("mutation: unsupported op %q", operation.Op)
	}
	parts, err := parsePointer(operation.Path)
	if err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if len(parts) == 0 {
		if op == "remove" {
			return fmt.Errorf("mutation: cannot remove document root")
		}
		if op == "replace" || op == "add" {
			*root = deepCloneJSON(operation.Value)
			return nil
		}
	}
	updated, err := patchValue(*root, parts, operation.Op, operation.Value)
	if err != nil {
		return err
	}
	*root = updated
	return nil
}

func patchValue(current any, parts []string, op string, value any) (any, error) {
	if len(parts) == 0 {
		if strings.EqualFold(op, "remove") {
			return nil, fmt.Errorf("mutation: cannot remove document root")
		}
		return deepCloneJSON(value), nil
	}
	token := parts[0]
	last := len(parts) == 1
	switch typed := current.(type) {
	case map[string]any:
		if last {
			existing, exists := typed[token]
			switch strings.ToLower(op) {
			case "add":
				typed[token] = deepCloneJSON(value)
				return typed, nil
			case "replace":
				if !exists {
					return nil, fmt.Errorf("mutation: path %q missing for replace", pointerFromParts(parts))
				}
				_ = existing
				typed[token] = deepCloneJSON(value)
				return typed, nil
			case "remove":
				if !exists {
					return nil, fmt.Errorf("mutation: path %q missing for remove", pointerFromParts(parts))
				}
				delete(typed, token)
				return typed, nil
			}
		}
		child, exists := typed[token]
		if !exists {
			return nil, fmt.Errorf("mutation: path %q missing", pointerFromParts(parts))
		}
		updated, err := patchValue(child, parts[1:], op, value)
		if err != nil {
			return nil, err
		}
		typed[token] = updated
		return typed, nil
	case []any:
		index, appendIndex, err := parseArrayIndex(token, len(typed), last && strings.EqualFold(op, "add"))
		if err != nil {
			return nil, err
		}
		if last {
			switch strings.ToLower(op) {
			case "add":
				if appendIndex {
					index = len(typed)
				}
				if index < 0 || index > len(typed) {
					return nil, fmt.Errorf("mutation: array path %q is out of bounds", pointerFromParts(parts))
				}
				item := deepCloneJSON(value)
				typed = append(typed, nil)
				copy(typed[index+1:], typed[index:])
				typed[index] = item
				return typed, nil
			case "replace":
				if index < 0 || index >= len(typed) {
					return nil, fmt.Errorf("mutation: array path %q missing for replace", pointerFromParts(parts))
				}
				typed[index] = deepCloneJSON(value)
				return typed, nil
			case "remove":
				if index < 0 || index >= len(typed) {
					return nil, fmt.Errorf("mutation: array path %q missing for remove", pointerFromParts(parts))
				}
				copy(typed[index:], typed[index+1:])
				return typed[:len(typed)-1], nil
			}
		}
		if index < 0 || index >= len(typed) {
			return nil, fmt.Errorf("mutation: array path %q missing", pointerFromParts(parts))
		}
		updated, err := patchValue(typed[index], parts[1:], op, value)
		if err != nil {
			return nil, err
		}
		typed[index] = updated
		return typed, nil
	default:
		return nil, fmt.Errorf("mutation: path %q is not traversable", pointerFromParts(parts))
	}
}

func parsePointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("invalid JSON pointer %q", pointer)
	}
	raw := strings.Split(pointer[1:], "/")
	parts := make([]string, len(raw))
	for i, token := range raw {
		var b strings.Builder
		for j := 0; j < len(token); j++ {
			if token[j] != '~' {
				b.WriteByte(token[j])
				continue
			}
			if j+1 >= len(token) || (token[j+1] != '0' && token[j+1] != '1') {
				return nil, fmt.Errorf("invalid JSON pointer escape in %q", pointer)
			}
			if token[j+1] == '0' {
				b.WriteByte('~')
			} else {
				b.WriteByte('/')
			}
			j++
		}
		parts[i] = b.String()
	}
	return parts, nil
}

func parseArrayIndex(token string, length int, allowDash bool) (int, bool, error) {
	if token == "-" {
		if allowDash {
			return length, true, nil
		}
		return 0, false, fmt.Errorf("mutation: '-' array index is only valid for add")
	}
	if token == "" || (len(token) > 1 && token[0] == '0') {
		return 0, false, fmt.Errorf("mutation: invalid array index %q", token)
	}
	index := 0
	for i := 0; i < len(token); i++ {
		if token[i] < '0' || token[i] > '9' {
			return 0, false, fmt.Errorf("mutation: invalid array index %q", token)
		}
		index = index*10 + int(token[i]-'0')
		if index < 0 {
			return 0, false, fmt.Errorf("mutation: array index overflow")
		}
	}
	if index > length {
		return index, false, nil
	}
	return index, false, nil
}

func pointerFromParts(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		b.WriteByte('/')
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(part, "~", "~0"), "/", "~1"))
	}
	return b.String()
}

func deepCloneJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		clone := make(map[string]any, len(typed))
		for key, item := range typed {
			clone[key] = deepCloneJSON(item)
		}
		return clone
	case []any:
		clone := make([]any, len(typed))
		for i, item := range typed {
			clone[i] = deepCloneJSON(item)
		}
		return clone
	case json.RawMessage:
		return append(json.RawMessage(nil), typed...)
	default:
		return typed
	}
}

func jsonDiff(before, after any) Diff {
	entries := make([]Entry, 0)
	diffJSONValues("", before, after, &entries)
	return Diff{Changed: len(entries) != 0, Entries: entries}
}

func diffJSONValues(path string, before, after any, entries *[]Entry) {
	if reflect.DeepEqual(before, after) {
		return
	}
	bm, bok := before.(map[string]any)
	am, aok := after.(map[string]any)
	if bok && aok {
		keys := make([]string, 0, len(bm)+len(am))
		seen := make(map[string]struct{}, len(bm)+len(am))
		for key := range bm {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
		for key := range am {
			if _, exists := seen[key]; !exists {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			childPath := path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
			old, hasOld := bm[key]
			next, hasNext := am[key]
			switch {
			case !hasOld:
				*entries = append(*entries, Entry{Path: childPath, After: next, Kind: "add"})
			case !hasNext:
				*entries = append(*entries, Entry{Path: childPath, Before: old, Kind: "remove"})
			default:
				diffJSONValues(childPath, old, next, entries)
			}
		}
		return
	}
	bs, bok := before.([]any)
	as, aok := after.([]any)
	if bok && aok {
		limit := len(bs)
		if len(as) < limit {
			limit = len(as)
		}
		for i := 0; i < limit; i++ {
			diffJSONValues(fmt.Sprintf("%s/%d", path, i), bs[i], as[i], entries)
		}
		for i := limit; i < len(bs); i++ {
			*entries = append(*entries, Entry{Path: fmt.Sprintf("%s/%d", path, i), Before: bs[i], Kind: "remove"})
		}
		for i := limit; i < len(as); i++ {
			*entries = append(*entries, Entry{Path: fmt.Sprintf("%s/%d", path, i), After: as[i], Kind: "add"})
		}
		return
	}
	*entries = append(*entries, Entry{Path: path, Before: before, After: after, Kind: "replace"})
}
