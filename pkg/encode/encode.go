// Package encode renders a decoded value — what a relation's cell holds — as the one canonical text
// or JSON form every sink writes. A host writing omnisdk's rows itself, and every sink omnisdk has,
// render through here, so the same value is the same bytes wherever it lands.
//
// The rules:
//
//   - a number is never written in exponent form, and a json.Number is written exactly as it was
//     read, so a 64-bit id survives;
//   - output is deterministic: map keys sorted, the same input the same bytes on every platform;
//   - nothing is HTML-escaped, and the encoder adds no trailing newline — framing is the sink's;
//   - NaN and ±Inf have no JSON form and are written as null; NonFinite lets a sink warn once.
//
// It depends on the standard library only, so a host takes it without taking the engine.
package encode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"
)

// Text renders v as its canonical string. ok is false for nil — the caller decides whether that is a
// SQL NULL or an empty field — and for a non-finite number, which has no value to render.
func Text(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case json.Number:
		return x.String(), true
	case float64:
		return float(x, 64)
	case float32:
		return float(float64(x), 32)
	case int:
		return strconv.FormatInt(int64(x), 10), true
	case int8:
		return strconv.FormatInt(int64(x), 10), true
	case int16:
		return strconv.FormatInt(int64(x), 10), true
	case int32:
		return strconv.FormatInt(int64(x), 10), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case uint:
		return strconv.FormatUint(uint64(x), 10), true
	case uint8:
		return strconv.FormatUint(uint64(x), 10), true
	case uint16:
		return strconv.FormatUint(uint64(x), 10), true
	case uint32:
		return strconv.FormatUint(uint64(x), 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano), true
	}
	b, err := JSON(v)
	if err != nil {
		return fmt.Sprintf("%v", v), true
	}
	return string(b), true
}

// float is a finite float in positional notation, never exponent form.
func float(f float64, bits int) (string, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", false
	}
	return strconv.FormatFloat(f, 'f', -1, bits), true
}

// JSON renders v as compact JSON, objects and arrays nested rather than stringified.
func JSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := write(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func write(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case string:
		return str(b, x)
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case json.Number:
		if x == "" {
			b.WriteString("null")
			return nil
		}
		b.WriteString(x.String())
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		s, ok := Text(x)
		if !ok {
			s = "null"
		}
		b.WriteString(s)
	case time.Time:
		return str(b, x.UTC().Format(time.RFC3339Nano))
	case map[string]any:
		b.WriteByte('{')
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := str(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := write(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := write(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	default:
		// Anything else is what encoding/json makes of it, re-read and written by these rules, so a
		// struct's floats and keys follow them too.
		raw, err := marshal(x)
		if err != nil {
			return str(b, fmt.Sprintf("%v", x))
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var back any
		if err := dec.Decode(&back); err != nil {
			return err
		}
		return write(b, back)
	}
	return nil
}

// str writes s as a JSON string, without HTML escaping.
func str(b *bytes.Buffer, s string) error {
	raw, err := marshal(s)
	if err != nil {
		return err
	}
	b.Write(raw)
	return nil
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// AsInt64 reports n as an int64 where it is an integer that fits — what an OTel int value takes.
func AsInt64(n json.Number) (int64, bool) {
	i, err := strconv.ParseInt(n.String(), 10, 64)
	return i, err == nil
}

// NonFinite reports whether v holds NaN or ±Inf anywhere, which JSON cannot carry and the encoder
// writes as null. A sink checks it to warn once per relation rather than once per row.
func NonFinite(v any) bool {
	switch x := v.(type) {
	case float64:
		return math.IsNaN(x) || math.IsInf(x, 0)
	case float32:
		return NonFinite(float64(x))
	case map[string]any:
		for _, e := range x {
			if NonFinite(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if NonFinite(e) {
				return true
			}
		}
	}
	return false
}
