package v1alpha1

import "testing"

// TestValidRunID is #244's table-driven acceptance test for the shared
// containment primitive: every boundary that joins a run id onto a
// directory (journal.Create, worktree.Manager.Create, `goobers trace`,
// `goobers run abort`) must refuse the identical set of traversal shapes,
// mirroring redact_test.go's containment cases for blob paths.
func TestValidRunID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"", false},
		{".", false},
		{"..", false},
		{"../../etc", false},
		{"/abs", false},
		{"/etc/passwd", false},
		{"a/b", false},
		{"a/../../b", false},
		{"run-123", true},
		{"0af7651916cd43dd8448eb211c80319c", true},
		{"replay-TestFoo-second-replay", true},
	}
	for _, c := range cases {
		if got := ValidRunID(c.id); got != c.want {
			t.Errorf("ValidRunID(%q) = %v, want %v", c.id, got, c.want)
		}
	}
}
