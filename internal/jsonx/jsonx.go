// Package jsonx has small helpers for working with decoded JSON values.
package jsonx

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Get reads a dot path ("result.payment_id", "items.0.id") from a decoded JSON value.
func Get(v any, path string) (any, bool) {
	cur := v
	for _, key := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[key]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// Set writes a dot path into a JSON object, creating intermediate objects.
func Set(m map[string]any, path string, value any) {
	keys := strings.Split(path, ".")
	cur := m
	for _, key := range keys[:len(keys)-1] {
		next, ok := cur[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[key] = next
		}
		cur = next
	}
	cur[keys[len(keys)-1]] = value
}

// Delete removes a dot path from a JSON object if present.
func Delete(m map[string]any, path string) {
	keys := strings.Split(path, ".")
	cur := m
	for _, key := range keys[:len(keys)-1] {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return
		}
		cur = next
	}
	delete(cur, keys[len(keys)-1])
}

// Normalize round-trips a value through JSON so YAML-decoded and wire-decoded
// values compare the same way (ints become float64, maps become map[string]any).
func Normalize(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}

// Key returns a canonical string for equality comparison of JSON values.
func Key(v any) string {
	b, err := json.Marshal(Normalize(v))
	if err != nil {
		return ""
	}
	return string(b)
}

// Equal compares two values as JSON.
func Equal(a, b any) bool { return Key(a) == Key(b) }

// ToFloat converts a JSON number (or numeric string) to float64.
func ToFloat(v any) (float64, bool) {
	switch n := Normalize(v).(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// Decode unmarshals raw JSON into a generic value; invalid JSON yields nil.
func Decode(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}
