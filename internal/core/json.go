package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Values in the run tree are JSON: step outputs, state, inputs, payloads.
// Whatever a store keeps is read back from JSON, so a value fresh from a node
// and the same value read after a restart must look the same to an expression.
// The convention, shared by every store and by the executor:
//
//   - objects are map[string]any, arrays []any, strings string, booleans bool,
//     null nil;
//   - a number written as an integer (no fraction, no exponent) that fits is
//     int64; every other number is float64.
//
// That is what CEL expects (int and double are different types), and what
// DecodeJSONValue returns. The executor passes node output through
// NormalizeJSON before it stores or reads it, so a resumed run sees what an
// uninterrupted one did.

// DecodeJSONValue decodes one JSON document into the generic form above.
func DecodeJSONValue(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF { //nolint:errorlint // io.EOF is the sentinel
		return nil, fmt.Errorf("decode json: trailing data")
	}
	return convertNumbers(v), nil
}

// NormalizeJSON round-trips v through JSON into the generic form: the value
// as a store would return it. A value that does not marshal is an error.
func NormalizeJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("normalize json: %w", err)
	}
	return DecodeJSONValue(b)
}

// JSONSize returns the length of v's JSON encoding, for the size limits of
// state and outputs.
func JSONSize(v any) (int, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, fmt.Errorf("json size: %w", err)
	}
	return len(b), nil
}

func convertNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		s := t.String()
		if !strings.ContainsAny(s, ".eE") {
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return i
			}
		}
		f, err := t.Float64()
		if err != nil {
			return s
		}
		return f
	case map[string]any:
		for k, x := range t {
			t[k] = convertNumbers(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = convertNumbers(x)
		}
		return t
	}
	return v
}
