// Package jcs implements the JSON Canonicalization Scheme of RFC 8785.
//
// Canonical bytes are what a content digest commits to when two producers must
// agree on the hash of the same logical JSON value regardless of whitespace,
// member order, or number spelling. The DSL 3.1 map roster (#1310) is the first
// consumer: an item digest is SHA-256 over the canonical form of
// {identity, item}.
//
// The input must be I-JSON (RFC 7493): valid UTF-8, no duplicate member names,
// no lone surrogate escapes, and numbers that are finite IEEE 754 doubles.
// Anything else is rejected rather than guessed at.
package jcs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// maxDepth bounds nesting independently of byte size, so a deeply nested
// document cannot exhaust the stack.
const maxDepth = 64

// ErrInvalid reports input that is not I-JSON and therefore has no canonical
// form.
var ErrInvalid = errors.New("jcs: input is not canonicalizable I-JSON")

// Canonicalize returns the RFC 8785 serialization of one JSON document.
func Canonicalize(data []byte) ([]byte, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: invalid UTF-8", ErrInvalid)
	}
	if err := rejectLoneSurrogates(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var out bytes.Buffer
	if err := writeValue(&out, decoder, 0); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data", ErrInvalid)
	}
	return out.Bytes(), nil
}

// Marshal encodes v with encoding/json and canonicalizes the result.
func Marshal(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Canonicalize(data)
}

func writeValue(out *bytes.Buffer, decoder *json.Decoder, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: nesting exceeds %d", ErrInvalid, maxDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			return writeObject(out, decoder, depth)
		case '[':
			return writeArray(out, decoder, depth)
		default:
			return fmt.Errorf("%w: unexpected %q", ErrInvalid, value)
		}
	case string:
		writeString(out, value)
	case json.Number:
		formatted, err := formatNumber(value)
		if err != nil {
			return err
		}
		out.WriteString(formatted)
	case bool:
		out.WriteString(strconv.FormatBool(value))
	case nil:
		out.WriteString("null")
	default:
		return fmt.Errorf("%w: unexpected token %T", ErrInvalid, token)
	}
	return nil
}

type member struct {
	key   string
	units []uint16
	value []byte
}

func writeObject(out *bytes.Buffer, decoder *json.Decoder, depth int) error {
	var members []member
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		key, ok := token.(string)
		if !ok {
			return fmt.Errorf("%w: non-string member name", ErrInvalid)
		}
		if seen[key] {
			return fmt.Errorf("%w: duplicate member %q", ErrInvalid, key)
		}
		seen[key] = true
		var value bytes.Buffer
		if err := writeValue(&value, decoder, depth+1); err != nil {
			return err
		}
		members = append(members, member{key: key, units: utf16.Encode([]rune(key)), value: value.Bytes()})
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	// RFC 8785 §3.2.3: members sort by their names' UTF-16 code units, which
	// differs from UTF-8 byte order for characters above U+FFFF.
	sort.Slice(members, func(i, j int) bool { return lessUTF16(members[i].units, members[j].units) })
	out.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			out.WriteByte(',')
		}
		writeString(out, m.key)
		out.WriteByte(':')
		out.Write(m.value)
	}
	out.WriteByte('}')
	return nil
}

func writeArray(out *bytes.Buffer, decoder *json.Decoder, depth int) error {
	out.WriteByte('[')
	for first := true; decoder.More(); first = false {
		if !first {
			out.WriteByte(',')
		}
		if err := writeValue(out, decoder, depth+1); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	out.WriteByte(']')
	return nil
}

func lessUTF16(a, b []uint16) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// writeString applies the ECMAScript JSON.stringify escaping RFC 8785 §3.2.2.2
// requires: only the quote, the backslash and C0 controls are escaped, the
// controls with a two-character form use it, and every other code point is
// written literally.
func writeString(out *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	out.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte(hex[r>>4])
				out.WriteByte(hex[r&0xf])
				continue
			}
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
}

// formatNumber serializes a JSON number as ECMAScript Number.prototype.toString
// does for the IEEE 754 double it denotes (RFC 8785 §3.2.2.3).
func formatNumber(number json.Number) (string, error) {
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return "", fmt.Errorf("%w: number %s is not a finite double", ErrInvalid, number)
	}
	if value == 0 {
		return "0", nil // includes negative zero
	}
	sign := ""
	if value < 0 {
		sign, value = "-", -value
	}
	// Shortest round-tripping digits and decimal exponent: value = 0.d1d2...dk × 10^n.
	mantissa, exponent, _ := strings.Cut(strconv.FormatFloat(value, 'e', -1, 64), "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	k := len(digits)
	e, err := strconv.Atoi(exponent)
	if err != nil {
		return "", fmt.Errorf("%w: number %s", ErrInvalid, number)
	}
	n := e + 1
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k), nil
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:], nil
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits, nil
	}
	expSign := "+"
	if n-1 < 0 {
		expSign = "-"
	}
	exp := strconv.Itoa(abs(n - 1))
	if k == 1 {
		return sign + digits + "e" + expSign + exp, nil
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + expSign + exp, nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// rejectLoneSurrogates refuses \uD800-\uDFFF escapes that do not form a valid
// high-low pair. encoding/json would silently replace them with U+FFFD, which
// would let two different documents share one canonical form.
func rejectLoneSurrogates(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			if i+1 >= len(data) {
				return nil // malformed; the decoder reports it
			}
			if data[i+1] != 'u' {
				i++
				continue
			}
			unit, ok := hexUnit(data, i+2)
			if !ok {
				return nil // malformed; the decoder reports it
			}
			i += 5
			switch {
			case unit >= 0xD800 && unit <= 0xDBFF:
				if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
					return fmt.Errorf("%w: unpaired surrogate escape", ErrInvalid)
				}
				low, ok := hexUnit(data, i+3)
				if !ok || low < 0xDC00 || low > 0xDFFF {
					return fmt.Errorf("%w: unpaired surrogate escape", ErrInvalid)
				}
				i += 6
			case unit >= 0xDC00 && unit <= 0xDFFF:
				return fmt.Errorf("%w: unpaired surrogate escape", ErrInvalid)
			}
		}
	}
	return nil
}

func hexUnit(data []byte, start int) (uint16, bool) {
	if start+4 > len(data) {
		return 0, false
	}
	value, err := strconv.ParseUint(string(data[start:start+4]), 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(value), true
}
