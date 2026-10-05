package eventing

import (
	"strings"
	"testing"
)

const example = `{"specversion":"1.0","id":"42","source":"/factory/test","type":"pr.changed","data":{"n":9007199254740993,"ready":true}}`

func TestEnvelopeCanonicalRetryAndAuthority(t *testing.T) {
	first, err := Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	reordered := `{"type":"pr.changed", "data":{"ready":true,"n":9007199254740993},"source":"/factory/test","id":"42","specversion":"1.0"}`
	second, err := Parse([]byte(reordered))
	if err != nil || first.Digest != second.Digest || first.ID != "42" {
		t.Fatalf("reordered retry: %+v, %v", second, err)
	}
	if !strings.Contains(string(first.JSON), "9007199254740993") {
		t.Fatal("event integer lost precision")
	}
	changed, err := Parse([]byte(strings.Replace(example, "9007199254740993", "9007199254740992", 1)))
	if err != nil || changed.Digest == first.Digest {
		t.Fatal("changed data did not change receipt digest")
	}
}

func TestEnvelopeRejectsAmbiguousAndUnboundedInputs(t *testing.T) {
	cases := []string{
		strings.Replace(example, `"id":"42"`, `"id":"42","id":"43"`, 1),
		strings.Replace(example, `"ready":true`, `"ready":true,"ready":false`, 1),
		strings.Replace(example, `"id":"42"`, `"Id":"42"`, 1),
		strings.Replace(example, `"source":"/factory/test"`, `"source":"bad uri"`, 1),
		strings.Replace(example, `"id":"42"`, `"id":"\ud800"`, 1),
		strings.Replace(example, `"id":"42"`, `"id":"a\n42"`, 1),
		strings.Replace(example, `"data":`, `"goobersgaggle":"foreign","data":`, 1),
		strings.Replace(example, `"data":`, `"datacontenttype":"text/plain","data":`, 1),
		strings.Replace(example, `"data":`, `"count":2147483648,"data":`, 1),
		strings.Replace(example, `"data":`, `"time":"tomorrow","data":`, 1),
		strings.Replace(example, `"data":`, `"dataschema":"/relative","data":`, 1),
		strings.Replace(example, `"data":`, `"extension":{},"data":`, 1),
		example + `{}`,
		strings.Repeat(" ", MaxEnvelopeBytes) + example,
		strings.Replace(example, `"data":`, `"nested":`+strings.Repeat("[", 34)+"0"+strings.Repeat("]", 34)+`,"data":`, 1),
	}
	for _, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid input: %.120s", raw)
		}
	}
}

func TestEnvelopePreservesExtensionAndNestedNonAuthorityData(t *testing.T) {
	raw := strings.Replace(example, `"data":`, `"traceparent":"abc","sampled":true,"count":3,"data":`, 1)
	if _, err := Parse([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	raw = strings.Replace(example, `"ready":true`, `"gaggle":"untrusted","ready":true`, 1)
	if _, err := Parse([]byte(raw)); err != nil {
		t.Fatal(err)
	}
}
