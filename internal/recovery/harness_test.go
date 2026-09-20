package recovery

import "testing"

func TestHarnessOwnedPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"mutations.jsonl", true},
		{"claimed-item.json", true},
		{"claimed-items.json", true},
		{".goobers/state.json", true},
		{".goobers", true},
		{"nested/.goobers/cache/item.json", true},
		// A repository may legitimately track a file of the same name in a
		// subdirectory. The harness never writes there, so it is content.
		{"docs/mutations.jsonl", false},
		{"pkg/claimed-item.json", false},
		{"mutations.jsonl.bak", false},
		{"goobers/state.json", false},
		{"README.md", false},
		{"", false},
	}
	for _, testCase := range cases {
		if got := HarnessOwnedPath(testCase.path); got != testCase.want {
			t.Errorf("HarnessOwnedPath(%q) = %v, want %v", testCase.path, got, testCase.want)
		}
	}
}

func TestBookkeepingOnlySnapshot(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		want  bool
	}{
		{name: "only harness artifacts", paths: []string{"mutations.jsonl", ".goobers/claim.json"}, want: true},
		{name: "one content path keeps the whole snapshot", paths: []string{"mutations.jsonl", "src/main.go"}, want: false},
		// An empty listing is the separate no-diff case, and an unavailable
		// listing must never be read as proof there was nothing to keep.
		{name: "empty listing is not bookkeeping-only", paths: nil, want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := BookkeepingOnlySnapshot(testCase.paths); got != testCase.want {
				t.Errorf("BookkeepingOnlySnapshot(%q) = %v, want %v", testCase.paths, got, testCase.want)
			}
		})
	}
}

func TestRecordHasNoDiff(t *testing.T) {
	if !(Record{PatchDigest: emptyPatchDigest}).HasNoDiff() {
		t.Error("a record carrying the empty-patch digest must report no diff")
	}
	if (Record{PatchDigest: "sha256:" + "a1"}).HasNoDiff() {
		t.Error("a record carrying real patch bytes must never report no diff")
	}
}
