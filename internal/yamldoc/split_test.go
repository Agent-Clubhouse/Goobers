package yamldoc

import "testing"

func TestSplit(t *testing.T) {
	raw := "apiVersion: v1\nkind: First\nmetadata:\n  name: first\n---\napiVersion: v1\nkind: Second\nmetadata:\n  name: second\n"
	docs := Split(raw)
	if len(docs) != 2 {
		t.Fatalf("Split() length = %d, want 2", len(docs))
	}
	if docs[0].Content == "" || docs[1].Content == "" {
		t.Fatalf("Split() returned empty document content: %#v", docs)
	}
	meta, ok, err := ExtractMetadata(docs[0].Content)
	if err != nil {
		t.Fatalf("ExtractMetadata() error = %v", err)
	}
	if !ok {
		t.Fatal("ExtractMetadata() reported no kind")
	}
	if meta.Kind != "First" || meta.Metadata.Name != "first" {
		t.Fatalf("ExtractMetadata() = %#v, want kind First and name first", meta)
	}
}
