package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// flatten turns a decoded JSON value (read with UseNumber) into string attributes. Nested objects become
// dotted keys ("CpuOptions.Core"), arrays of scalars are joined with commas, and other arrays are kept as
// compact JSON. Nulls, empty strings, empty objects and empty arrays are left out. Numbers keep the text
// the provider wrote, so "4.0" stays "4.0".
func flatten(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case nil:
	case string:
		if x != "" {
			out[prefix] = x
		}
	case json.Number:
		out[prefix] = x.String()
	case bool:
		if x {
			out[prefix] = "true"
		} else {
			out[prefix] = "false"
		}
	case map[string]any:
		for k, e := range x {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flatten(key, e, out)
		}
	case []any:
		if len(x) == 0 {
			return
		}
		parts := make([]string, 0, len(x))
		for _, e := range x {
			switch s := e.(type) {
			case string:
				parts = append(parts, s)
			case json.Number:
				parts = append(parts, s.String())
			default:
				out[prefix] = compactJSON(x)
				return
			}
		}
		out[prefix] = strings.Join(parts, ",")
	default:
		out[prefix] = compactJSON(x)
	}
}

func compactJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(buf.String(), "\n")
}

// decodeNumbers decodes JSON keeping numbers as their source text.
func decodeNumbers(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(dst)
}
