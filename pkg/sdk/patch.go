package sdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
)

// Operation is one RFC6902 JSON Patch operation. Value is the operation's
// JSON value; it is ignored for "remove" and required for "add", "replace",
// and "test". Decoding a patch document leaves Value as the generic JSON
// representation (maps, slices, strings, float64, bool, or nil).
type Operation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// Apply applies RFC6902 operations to a JSON document and returns the patched
// document. Supported operations are "add", "replace", "remove", and "test";
// paths are JSON Pointers (RFC6901), including "/a/b/0" array indexes and the
// "~0"/"~1" escapes. The input document is not modified.
//
// Operations apply in order. If any operation fails, Apply returns an error and
// no partial result.
func Apply(doc []byte, ops []Operation) ([]byte, error) {
	root, err := decodeDocument(doc)
	if err != nil {
		return nil, err
	}

	for i, op := range ops {
		root, err = applyOperation(root, op)
		if err != nil {
			return nil, fmt.Errorf("sdk: patch op %d (%s %q): %w", i+1, op.Op, op.Path, err)
		}
	}

	patched, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("sdk: encode patched document: %w", err)
	}
	return patched, nil
}

// decodeDocument parses exactly one JSON value from doc.
func decodeDocument(doc []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(doc))

	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("sdk: invalid patch document: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("sdk: invalid patch document: trailing data")
	}
	return root, nil
}

// applyOperation validates one operation and dispatches to applyAt.
func applyOperation(root any, op Operation) (any, error) {
	tokens, err := parsePointer(op.Path)
	if err != nil {
		return nil, err
	}

	switch op.Op {
	case "add", "replace", "test":
		value, err := normalizeValue(op.Value)
		if err != nil {
			return nil, err
		}
		return applyAt(root, tokens, op, value)

	case "remove":
		return applyAt(root, tokens, op, nil)

	default:
		return nil, fmt.Errorf("unsupported op %q", op.Op)
	}
}

// applyAt walks tokens from node and returns the (possibly replaced) subtree.
// It is functional rather than pointer-based so array resizes do not need a
// separate setter.
func applyAt(node any, tokens []string, op Operation, value any) (any, error) {
	if len(tokens) == 0 {
		switch op.Op {
		case "add", "replace":
			return value, nil
		case "test":
			if !reflect.DeepEqual(node, value) {
				return nil, fmt.Errorf("test failed at %q", op.Path)
			}
			return node, nil
		default: // remove
			return nil, fmt.Errorf("cannot remove the document root %q", op.Path)
		}
	}

	token := tokens[0]

	switch container := node.(type) {
	case map[string]any:
		return applyToMap(container, token, tokens, op, value)

	case []any:
		return applyToArray(container, token, tokens, op, value)

	default:
		return nil, fmt.Errorf("path %q not found", op.Path)
	}
}

func applyToMap(container map[string]any, token string, tokens []string, op Operation, value any) (any, error) {
	child, exists := container[token]

	if len(tokens) > 1 {
		if !exists {
			return nil, fmt.Errorf("path %q not found", op.Path)
		}
		updated, err := applyAt(child, tokens[1:], op, value)
		if err != nil {
			return nil, err
		}
		container[token] = updated
		return container, nil
	}

	switch op.Op {
	case "add":
		container[token] = value
		return container, nil

	case "replace":
		if !exists {
			return nil, fmt.Errorf("path %q not found", op.Path)
		}
		container[token] = value
		return container, nil

	case "remove":
		if !exists {
			return nil, fmt.Errorf("path %q not found", op.Path)
		}
		delete(container, token)
		return container, nil

	case "test":
		if !exists {
			return nil, fmt.Errorf("path %q not found", op.Path)
		}
		if !reflect.DeepEqual(child, value) {
			return nil, fmt.Errorf("test failed at %q", op.Path)
		}
		return container, nil
	}

	return nil, fmt.Errorf("unsupported op %q", op.Op)
}

func applyToArray(container []any, token string, tokens []string, op Operation, value any) (any, error) {
	leaf := len(tokens) == 1

	// "-" is only meaningful as the final token of an add operation.
	idx, err := parseIndex(token, len(container), leaf && op.Op == "add")
	if err != nil {
		return nil, fmt.Errorf("%w at %q", err, op.Path)
	}

	if !leaf {
		updated, err := applyAt(container[idx], tokens[1:], op, value)
		if err != nil {
			return nil, err
		}
		container[idx] = updated
		return container, nil
	}

	switch op.Op {
	case "add":
		container = append(container, nil)
		copy(container[idx+1:], container[idx:])
		container[idx] = value
		return container, nil

	case "replace":
		container[idx] = value
		return container, nil

	case "remove":
		return append(container[:idx], container[idx+1:]...), nil

	case "test":
		if !reflect.DeepEqual(container[idx], value) {
			return nil, fmt.Errorf("test failed at %q", op.Path)
		}
		return container, nil
	}

	return nil, fmt.Errorf("unsupported op %q", op.Op)
}

// parseIndex validates an RFC6902 array index. add may use "-" or len(array)
// to append; every other operation must reference an existing element.
func parseIndex(token string, length int, allowAppend bool) (int, error) {
	if token == "-" {
		if allowAppend {
			return length, nil
		}
		return 0, errors.New("array index \"-\" is only valid as the last token of add")
	}

	// RFC6902 indexes are base-10 without leading zeros.
	if token == "" || (len(token) > 1 && token[0] == '0') {
		return 0, fmt.Errorf("invalid array index %q", token)
	}

	index, err := strconv.Atoi(token)
	if err != nil || index < 0 {
		return 0, fmt.Errorf("invalid array index %q", token)
	}

	upper := length - 1
	if allowAppend {
		upper = length
	}
	if index > upper {
		return 0, fmt.Errorf("array index %d out of range", index)
	}
	return index, nil
}

// parsePointer splits an RFC6901 JSON Pointer into unescaped reference tokens.
func parsePointer(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("invalid JSON pointer %q: must be empty or start with '/'", path)
	}

	parts := strings.Split(path[1:], "/")
	tokens := make([]string, len(parts))
	for i, part := range parts {
		token, err := unescapeToken(part)
		if err != nil {
			return nil, fmt.Errorf("invalid JSON pointer %q: %w", path, err)
		}
		tokens[i] = token
	}
	return tokens, nil
}

// unescapeToken applies the RFC6901 "~1" -> "/" and "~0" -> "~" escapes.
func unescapeToken(token string) (string, error) {
	if !strings.Contains(token, "~") {
		return token, nil
	}

	var b strings.Builder
	for i := 0; i < len(token); i++ {
		if token[i] != '~' {
			b.WriteByte(token[i])
			continue
		}
		if i+1 >= len(token) {
			return "", errors.New("dangling '~'")
		}
		switch token[i+1] {
		case '0':
			b.WriteByte('~')
		case '1':
			b.WriteByte('/')
		default:
			return "", fmt.Errorf("invalid escape %q", token[i:i+2])
		}
		i++
	}
	return b.String(), nil
}

// normalizeValue round-trips a value through encoding/json so it has the same
// concrete types as the decoded document (notably float64 for numbers). This
// keeps "test" comparisons and values added by later operations consistent.
func normalizeValue(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode patch value: %w", err)
	}

	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, fmt.Errorf("decode patch value: %w", err)
	}
	return normalized, nil
}
