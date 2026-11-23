// Package pyjson re-serializes JSON exactly as Python's json.dumps does with
// its defaults: object keys in their original order, separators ", " and
// ": ", and ensure_ascii (non-ASCII as \uXXXX escapes, surrogate pairs above
// the BMP). Numbers keep their source text. The original Glance stores
// metadata definitions with json.dumps; this reproduces the same bytes.
package pyjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Dumps returns the json.dumps text of one JSON value.
func Dumps(raw []byte) (string, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var b bytes.Buffer
	if err := value(d, &b); err != nil {
		return "", err
	}
	if _, err := d.Token(); err != io.EOF {
		return "", errors.New("trailing data after JSON value")
	}
	return b.String(), nil
}

func value(d *json.Decoder, b *bytes.Buffer) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	return token(d, b, t)
}

func token(d *json.Decoder, b *bytes.Buffer, t json.Token) error {
	switch v := t.(type) {
	case json.Delim:
		switch v {
		case '{':
			b.WriteByte('{')
			for first := true; d.More(); first = false {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := k.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if !first {
					b.WriteString(", ")
				}
				writeString(b, key)
				b.WriteString(": ")
				if err = value(d, b); err != nil {
					return err
				}
			}
			if _, err := d.Token(); err != nil {
				return err
			}
			b.WriteByte('}')
		case '[':
			b.WriteByte('[')
			for first := true; d.More(); first = false {
				if !first {
					b.WriteString(", ")
				}
				if err := value(d, b); err != nil {
					return err
				}
			}
			if _, err := d.Token(); err != nil {
				return err
			}
			b.WriteByte(']')
		default:
			return fmt.Errorf("unexpected delimiter %v", v)
		}
	case string:
		writeString(b, v)
	case json.Number:
		n, err := number(v.String())
		if err != nil {
			return err
		}
		b.WriteString(n)
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case nil:
		b.WriteString("null")
	default:
		return fmt.Errorf("unexpected token %T", t)
	}
	return nil
}

// writeString is Python's ensure_ascii string encoder (py_encode_basestring_ascii).
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20 || r >= 0x7f && r < 0x10000:
				fmt.Fprintf(b, `\u%04x`, r)
			case r >= 0x10000:
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(b, `\u%04x\u%04x`, hi, lo)
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// Members returns the raw values of an object's members in source order.
func Members(raw []byte) ([]string, map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	t, err := d.Token()
	if err != nil {
		return nil, nil, err
	}
	if t != json.Delim('{') {
		return nil, nil, errors.New("not a JSON object")
	}
	var keys []string
	values := map[string]json.RawMessage{}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return nil, nil, err
		}
		key := k.(string)
		var v json.RawMessage
		if err = d.Decode(&v); err != nil {
			return nil, nil, err
		}
		if _, dup := values[key]; !dup {
			keys = append(keys, key)
		}
		values[key] = v
	}
	return keys, values, nil
}

// number: an integer keeps its text (Python int); any other number is a
// Python float written by repr: shortest round-trip digits, fixed notation
// for 1e-4 <= |x| < 1e16 (always with a fractional part), else exponent.
func number(text string) (string, error) {
	if !strings.ContainsAny(text, ".eE") {
		return text, nil
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return "", err
	}
	if f == 0 {
		if strings.HasPrefix(text, "-") {
			return "-0.0", nil
		}
		return "0.0", nil
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // -d.ddde±XX
	sign := ""
	if e[0] == '-' {
		sign, e = "-", e[1:]
	}
	mant, expText, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mant, ".", "", 1)
	if exp >= -4 && exp < 16 {
		var out string
		if exp >= 0 {
			if len(digits) <= exp+1 {
				out = digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
			} else {
				out = digits[:exp+1] + "." + digits[exp+1:]
			}
		} else {
			out = "0." + strings.Repeat("0", -exp-1) + digits
		}
		return sign + out, nil
	}
	m := digits[:1]
	if len(digits) > 1 {
		m += "." + digits[1:]
	}
	es := "+"
	if exp < 0 {
		es, exp = "-", -exp
	}
	return fmt.Sprintf("%s%se%s%02d", sign, m, es, exp), nil
}
