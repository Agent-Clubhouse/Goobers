package recovery

import (
	"bytes"
	"testing"
)

func TestChildSnapshotPolicyBoundsAndFiltering(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../token", "/token", "a/../b", "a\\b", "*.key", "a\n"} {
		if err := (SnapshotPolicy{ExcludedPaths: []string{name}}).Validate(); err == nil {
			t.Errorf("accepted invalid exclusion %q", name)
		}
	}
	if err := (SnapshotPolicy{ExcludedPaths: make([]string, 129)}).Validate(); err == nil {
		t.Fatal("accepted unbounded exclusions")
	}
	policy := SnapshotPolicy{ExcludedPaths: []string{"private/token"}}
	for _, name := range []string{".goobers/secret", ".GoObErS/config", ".goober-assets/x", ".git", ".goobers-launcher-session-1/socket", "mutations.jsonl", "private/token"} {
		if !policy.excludes(name) {
			t.Errorf("did not exclude %q", name)
		}
	}
	for _, name := range []string{"main.go", ".github/workflows/test.yml", ".agents/skills/repository/SKILL.md", "private/tokenizer"} {
		if policy.excludes(name) {
			t.Errorf("excluded source %q", name)
		}
	}
	paths, err := filterSnapshotPaths([]byte("main.go\x00.goobers/key\x00private/token\x00"), &policy)
	if err != nil || !bytes.Equal(paths, []byte("main.go\x00")) {
		t.Fatalf("paths=%q err=%v", paths, err)
	}
	index := []byte("100644 abc 0\t.goobers/key\x00100644 abc 0\tmain.go\x00")
	filtered, err := filterSnapshotIndex(index, &policy)
	if err != nil || bytes.Contains(filtered, []byte("key")) {
		t.Fatalf("index=%q err=%v", filtered, err)
	}
	if unfiltered, _ := filterSnapshotIndex(index, nil); !bytes.Equal(unfiltered, index) {
		t.Fatal("legacy capture changed")
	}
}
