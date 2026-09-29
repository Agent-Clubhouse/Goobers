package handoffcheck

import (
	"strings"
	"testing"
)

const testSchema = `{
  "type": "object",
  "required": ["verdict", "items"],
  "additionalProperties": false,
  "properties": {
    "verdict": {"enum": ["pass", "fail"]},
    "items": {"type": "array", "items": {"type": "object", "required": ["id"], "properties": {"id": {"type": "integer"}}}}
  }
}`

func mustSchema(t *testing.T) *Schema {
	t.Helper()
	s, err := Compile("test.handoff", "1", []byte(testSchema))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func codes(v Verdict) []string {
	var out []string
	for _, i := range v.Issues {
		out = append(out, i.Code)
	}
	return out
}

func TestCheckValid(t *testing.T) {
	v := mustSchema(t).Check([]byte(`{"verdict":"pass","items":[{"id":1}]}`))
	if !v.Valid || len(v.Issues) != 0 {
		t.Fatalf("expected valid, got %+v", v)
	}
	if v.SchemaID != "test.handoff" || v.SchemaVersion != "1" || v.Digest == "" || v.Size == 0 {
		t.Fatalf("verdict identity missing: %+v", v)
	}
}

func TestCheckIsDeterministic(t *testing.T) {
	s := mustSchema(t)
	in := []byte(`{"verdict":"maybe","items":[{"id":"x"},{}],"extra":1}`)
	first := s.Check(in)
	for i := 0; i < 50; i++ {
		next := s.Check(in)
		if len(next.Issues) != len(first.Issues) {
			t.Fatalf("issue count changed: %d vs %d", len(next.Issues), len(first.Issues))
		}
		for j := range next.Issues {
			if next.Issues[j] != first.Issues[j] {
				t.Fatalf("issue %d changed: %+v vs %+v", j, next.Issues[j], first.Issues[j])
			}
		}
	}
}

func TestSyntaxFailures(t *testing.T) {
	cases := []struct {
		name, in, code string
	}{
		{"empty", "  \n", CodeEmpty},
		{"truncated object", `{"verdict":"pass","items":[{"id":1}`, CodeTruncated},
		{"truncated string", `{"verdict":"pa`, CodeTruncated},
		{"trailing data", `{"verdict":"pass","items":[]} {"x":1}`, CodeTrailingData},
		{"trailing prose", `{"verdict":"pass","items":[]} hope that helps`, CodeInvalidJSON},
		{"duplicate key", `{"verdict":"pass","verdict":"fail","items":[]}`, CodeDuplicateKey},
		{"code fence", "```json\n{\"verdict\":\"pass\"}\n```", CodeNotJSONDocument},
		{"prose", `Here is the result: {"verdict":"pass"}`, CodeNotJSONDocument},
		{"bad token", `{"verdict": pass}`, CodeInvalidJSON},
	}
	s := mustSchema(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := s.Check([]byte(tc.in))
			if v.Valid || len(v.Issues) == 0 || v.Issues[0].Code != tc.code {
				t.Fatalf("want %s, got valid=%v issues=%+v", tc.code, v.Valid, v.Issues)
			}
		})
	}
}

func TestDuplicateKeyPathAndEscape(t *testing.T) {
	v := CheckSyntax([]byte(`{"a/b":{"k":1,"k":2}}`))
	if v.Valid || v.Issues[0].Code != CodeDuplicateKey || v.Issues[0].Path != "/a~1b/k" {
		t.Fatalf("unexpected: %+v", v)
	}
}

func TestSchemaViolationsAreStructured(t *testing.T) {
	v := mustSchema(t).Check([]byte(`{"verdict":"maybe","items":[{"id":"x"},{}],"extra":1}`))
	if v.Valid {
		t.Fatal("expected invalid")
	}
	paths := map[string]bool{}
	for _, i := range v.Issues {
		if i.Code != CodeSchemaViolation || i.Message == "" {
			t.Fatalf("bad issue: %+v", i)
		}
		paths[i.Path] = true
	}
	for _, want := range []string{"/verdict", "/items/0/id", "/items/1"} {
		if !paths[want] {
			t.Errorf("missing issue at %s; got %+v", want, v.Issues)
		}
	}
}

func TestSchemaOnlyRunsAfterSyntaxPasses(t *testing.T) {
	v := mustSchema(t).Check([]byte(`{"verdict":"pass"`))
	if got := codes(v); len(got) != 1 || got[0] != CodeTruncated {
		t.Fatalf("got %v", got)
	}
}

func TestTooLarge(t *testing.T) {
	v := check(nil, []byte(strings.Repeat(" ", 10)+"{}"), 5)
	if v.Valid || v.Issues[0].Code != CodeTooLarge {
		t.Fatalf("unexpected: %+v", v)
	}
}

func TestCompileErrors(t *testing.T) {
	if _, err := Compile("", "1", []byte(`{}`)); err == nil {
		t.Fatal("expected error for empty id")
	}
	if _, err := Compile("x", "1", []byte(`{"type": 5}`)); err == nil {
		t.Fatal("expected error for invalid schema")
	}
}

func TestDigestTracksBytes(t *testing.T) {
	a := CheckSyntax([]byte(`{"a":1}`))
	b := CheckSyntax([]byte(`{"a":2}`))
	if a.Digest == b.Digest || !a.Valid || !b.Valid {
		t.Fatalf("digest must differ: %s %s", a.Digest, b.Digest)
	}
}
