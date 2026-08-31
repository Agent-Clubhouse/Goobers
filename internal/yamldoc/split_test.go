package yamldoc

import (
	"strings"
	"testing"
)

func TestSplitDocuments(t *testing.T) {
	raw := []byte(`---
kind: First
metadata:
  name: first
---` + "\n\t\n" + `---
metadata:
  name: no-kind
---
kind: [
---
kind: Second
dslVersion: "2.0"
metadata:
  name: second
`)

	docs := SplitDocuments(raw)
	if len(docs) != 2 {
		t.Fatalf("SplitDocuments() returned %d documents, want 2: %#v", len(docs), docs)
	}

	if got, want := docs[0].Meta, (Metadata{Kind: "First", Name: "first"}); got != want {
		t.Errorf("first document metadata = %#v, want %#v", got, want)
	}
	if !strings.Contains(string(docs[0].Content), "kind: First") {
		t.Errorf("first document content = %q, want raw First document", docs[0].Content)
	}

	if got, want := docs[1].Meta, (Metadata{Kind: "Second", Name: "second", DSLVersion: "2.0"}); got != want {
		t.Errorf("second document metadata = %#v, want %#v", got, want)
	}
	if !strings.Contains(string(docs[1].Content), "kind: Second") {
		t.Errorf("second document content = %q, want raw Second document", docs[1].Content)
	}
}

func TestSplitDocumentsEmptyInput(t *testing.T) {
	for _, raw := range [][]byte{nil, {}} {
		if docs := SplitDocuments(raw); docs != nil {
			t.Errorf("SplitDocuments(%q) = %#v, want nil", raw, docs)
		}
	}
}
