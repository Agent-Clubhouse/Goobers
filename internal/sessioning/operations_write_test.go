package sessioning

import (
	"strings"
	"testing"
)

func TestSessionNativeEditClosedArguments(t *testing.T) {
	valid := `{"sourceBindingId":"items","requestId":"edit-1","id":"42","sourceId":"99","expectedRevision":"1","field":"labels","values":[]}`
	request, err := DecodeBacklogEdit([]byte(valid))
	if err != nil || request.Values == nil {
		t.Fatal(request, err)
	}
	for _, raw := range []string{
		strings.Replace(valid, `"values":[]`, `"value":"x","values":[]`, 1),
		strings.Replace(valid, `"values":[]`, `"values":null`, 1),
		strings.Replace(valid, `"values":[]`, `"credentialRef":"secret","values":[]`, 1),
		strings.Replace(valid, `"values":[]`, `"values":[],"requestId":"other"`, 1),
		strings.Replace(valid, `"field":"labels"`, `"field":"needs-human"`, 1),
		strings.Replace(valid, `"id":"42"`, `"id":"../1"`, 1),
	} {
		if _, err := DecodeBacklogEdit([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	large := strings.Replace(valid, `"field":"labels","values":[]`, `"field":"description","value":"`+strings.Repeat("a", 20<<10)+`"`, 1)
	if _, err := DecodeBacklogEdit([]byte(large)); err != nil {
		t.Fatal("write incorrectly used read bound", err)
	}
	if _, err := DecodeBacklogEdit([]byte(strings.Replace(large, strings.Repeat("a", 20<<10), strings.Repeat("a", MaxOperationWriteBytes), 1))); err == nil {
		t.Fatal("unbounded write")
	}
}
