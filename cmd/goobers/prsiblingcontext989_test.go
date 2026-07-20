package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestGatherSiblingContextComputesFileOverlap is #989: gather-sibling-context
// computes, deterministically, the set of files each sibling shares with the
// selected PR — the ground-truth collision the sequencing classification
// (#990) consumes instead of relying on the LLM reviewer to notice it. A
// disjoint sibling has empty overlap and is absent from overlappingSiblings.
func TestGatherSiblingContextComputesFileOverlap(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")

	const (
		selectedNumber  = 10
		overlapSibling  = 11
		disjointSibling = 12
	)
	// Selected touches A, B, C.
	server.addIssue(selectedNumber, "Selected PR")
	server.addOpenPR(selectedNumber, "goobers/implementation/run-10", "main", "sha10", "base",
		false, nil, []fakePRFile{
			{path: "a.go", status: "modified"},
			{path: "b.go", status: "modified"},
			{path: "c.go", status: "modified"},
		})
	// Sibling #11 touches B, C, D → overlap {b.go, c.go}.
	server.addIssue(overlapSibling, "Overlapping sibling")
	server.addOpenPR(overlapSibling, "goobers/implementation/run-11", "main", "sha11", "base",
		false, nil, []fakePRFile{
			{path: "c.go", status: "modified"},
			{path: "b.go", status: "modified"},
			{path: "d.go", status: "modified"},
		})
	// Sibling #12 touches E, F → no overlap.
	server.addIssue(disjointSibling, "Disjoint sibling")
	server.addOpenPR(disjointSibling, "goobers/implementation/run-12", "main", "sha12", "base",
		false, nil, []fakePRFile{
			{path: "e.go", status: "modified"},
			{path: "f.go", status: "modified"},
		})

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "10")
	t.Setenv("GOOBERS_INPUT_NOVERDICTCACHE", "")
	dir := t.TempDir()
	t.Chdir(dir)

	if code, stdout, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sibling-context.json"))
	if err != nil {
		t.Fatalf("read sibling-context.json: %v", err)
	}
	var ctx struct {
		Siblings []struct {
			Number  int      `json:"number"`
			Overlap []string `json:"overlap"`
		} `json:"siblings"`
		OverlappingSiblings []int `json:"overlappingSiblings"`
	}
	if err := json.Unmarshal(data, &ctx); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	overlapByNumber := map[int][]string{}
	for _, s := range ctx.Siblings {
		overlapByNumber[s.Number] = s.Overlap
	}
	if got, want := overlapByNumber[overlapSibling], []string{"b.go", "c.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sibling #%d overlap = %v, want %v (sorted intersection)", overlapSibling, got, want)
	}
	if got := overlapByNumber[disjointSibling]; len(got) != 0 {
		t.Fatalf("sibling #%d overlap = %v, want empty (disjoint files)", disjointSibling, got)
	}
	if want := []int{overlapSibling}; !reflect.DeepEqual(ctx.OverlappingSiblings, want) {
		t.Fatalf("overlappingSiblings = %v, want %v", ctx.OverlappingSiblings, want)
	}
}
