package jcs

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"testing"
)

// TestCanonicalizeRFC8785Example is the worked example of RFC 8785 §3.2.4:
// whitespace, member order, number spelling and string escaping all change.
func TestCanonicalizeRFC8785Example(t *testing.T) {
	input := `{
  "numbers": [333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001],
  "string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/",
  "literals": [null, true, false]
}`
	want := `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"€$\u000f\nA'B\"\\\\\"/"}`
	got, err := Canonicalize([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("canonical form\n got %s\nwant %s", got, want)
	}
}

// TestCanonicalizeSortsByUTF16CodeUnits is the RFC 8785 §3.2.3 sorting
// example: U+1F600 sorts before U+FB33 by UTF-16 code units, the reverse of
// UTF-8 byte order.
func TestCanonicalizeSortsByUTF16CodeUnits(t *testing.T) {
	input := `{"\u20ac":"Euro Sign","\r":"Carriage Return","\ufb33":"Hebrew Letter Dalet With Dagesh","1":"One","\ud83d\ude00":"Emoji: Grinning Face","\u0080":"Control","\u00f6":"Latin Small Letter O With Diaeresis"}`
	want := "{\"\\r\":\"Carriage Return\",\"1\":\"One\",\"\u0080\":\"Control\",\"ö\":\"Latin Small Letter O With Diaeresis\",\"€\":\"Euro Sign\",\"😀\":\"Emoji: Grinning Face\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}"
	got, err := Canonicalize([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("canonical form\n got %s\nwant %s", got, want)
	}
}

// TestFormatNumberRFC8785Vectors pins the ECMAScript number serialization
// against the IEEE 754 vectors in RFC 8785 Appendix B.
func TestFormatNumberRFC8785Vectors(t *testing.T) {
	for bits, want := range map[uint64]string{
		0x0000000000000000: "0",
		0x8000000000000000: "0",
		0x0000000000000001: "5e-324",
		0x8000000000000001: "-5e-324",
		0x7fefffffffffffff: "1.7976931348623157e+308",
		0xffefffffffffffff: "-1.7976931348623157e+308",
		0x4340000000000000: "9007199254740992",
		0xc340000000000000: "-9007199254740992",
		0x4430000000000000: "295147905179352830000",
		0x44b52d02c7e14af5: "9.999999999999997e+22",
		0x44b52d02c7e14af6: "1e+23",
		0x44b52d02c7e14af7: "1.0000000000000001e+23",
		0x444b1ae4d6e2ef4e: "999999999999999700000",
		0x444b1ae4d6e2ef4f: "999999999999999900000",
		0x444b1ae4d6e2ef50: "1e+21",
		0x3eb0c6f7a0b5ed8c: "9.999999999999997e-7",
		0x3eb0c6f7a0b5ed8d: "0.000001",
		0x41b3de4355555553: "333333333.3333332",
		0x41b3de4355555554: "333333333.33333325",
		0x41b3de4355555555: "333333333.3333333",
		0x41b3de4355555556: "333333333.3333334",
		0x41b3de4355555557: "333333333.33333343",
		0xbecbf647612f3696: "-0.0000033333333333333333",
		0x43143ff3c1cb0959: "1424953923781206.2",
	} {
		value := math.Float64frombits(bits)
		got, err := formatNumber(json.Number(strconv.FormatFloat(value, 'g', -1, 64)))
		if err != nil {
			t.Fatalf("%#016x: %v", bits, err)
		}
		if got != want {
			t.Errorf("%#016x = %s, want %s", bits, got, want)
		}
	}
}

func TestCanonicalizeIsStableUnderReordering(t *testing.T) {
	a, err := Canonicalize([]byte(`{"b":[1,{"y":2,"x":1}],"a":"<&>"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonicalize([]byte(" { \"a\" : \"\\u003c&>\" , \"b\" : [ 1.0 , { \"x\" : 1e0 , \"y\" : 2 } ] } "))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) || string(a) != `{"a":"<&>","b":[1,{"x":1,"y":2}]}` {
		t.Fatalf("canonical forms differ: %s vs %s", a, b)
	}
	marshaled, err := Marshal(map[string]any{"b": []any{1, map[string]any{"y": 2, "x": 1}}, "a": "<&>"})
	if err != nil || string(marshaled) != string(a) {
		t.Fatalf("Marshal = %s, %v; want %s", marshaled, err, a)
	}
}

func TestCanonicalizeRejectsNonIJSON(t *testing.T) {
	for name, input := range map[string]string{
		"duplicate member":   `{"a":1,"a":2}`,
		"lone high":          `"\ud800"`,
		"lone low":           `"\udc00x"`,
		"high then non-low":  `"\ud800\u0041"`,
		"overflow":           `1e400`,
		"trailing data":      `{} {}`,
		"truncated":          `{"a":`,
		"invalid utf8":       "\"\xff\"",
		"excessive nesting":  nested(maxDepth + 2),
		"non-string key ish": `{1:2}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Canonicalize([]byte(input)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Canonicalize(%q) = %v, want ErrInvalid", input, err)
			}
		})
	}
	if got, err := Canonicalize([]byte(`"\ud83d\ude00"`)); err != nil || string(got) != `"😀"` {
		t.Fatalf("valid surrogate pair = %s, %v", got, err)
	}
}

func nested(depth int) string {
	out := ""
	for range depth {
		out += "["
	}
	for range depth {
		out += "]"
	}
	return out
}
