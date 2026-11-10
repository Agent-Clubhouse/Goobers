package blockedcycle

import (
	"fmt"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestCommentIsBounded(t *testing.T) {
	paths := make([][]string, 20)
	for i := range paths {
		for j := 0; j < 100; j++ {
			paths[i] = append(paths[i], strings.Repeat("9", 100))
		}
	}
	comment := Comment(paths, true)
	if len(comment) > MaxCommentLength {
		t.Fatalf("comment length = %d, want at most %d", len(comment), MaxCommentLength)
	}
	if !strings.Contains(comment, "additional cycle paths omitted") {
		t.Fatalf("comment = %q, want omitted-path notice", comment)
	}
	if !strings.Contains(comment, "cycle members omitted") {
		t.Fatalf("comment = %q, want explicit omitted-member notice", comment)
	}

	singleCycleComment := Comment(paths[:1], false)
	if len(singleCycleComment) > MaxCommentLength {
		t.Fatalf("single-cycle comment length = %d, want at most %d", len(singleCycleComment), MaxCommentLength)
	}
	if !strings.Contains(singleCycleComment, "cycle members omitted") {
		t.Fatalf("single-cycle comment = %q, want explicit omitted-member notice", singleCycleComment)
	}
	if strings.Contains(singleCycleComment, "additional cycle paths omitted") {
		t.Fatalf("single-cycle comment = %q, did not want omitted-path notice", singleCycleComment)
	}
}

func TestCommentPreservesLongSingleCycle(t *testing.T) {
	path := []string{
		"1001", "1002", "1003", "1004", "1005", "1006", "1007",
		"1008", "1009", "1010", "1011", "1012", "1013", "1014",
		"1015", "1016", "1017", "1018", "1019", "1020",
	}
	path = append(path, path[0])

	wantMembers := make([]string, len(path))
	for i, number := range path {
		wantMembers[i] = "#" + number
	}
	wantPath := strings.Join(wantMembers, pathSeparator)
	comment := Comment([][]string{path}, false)
	if !strings.Contains(comment, wantPath) {
		t.Fatalf("comment = %q, want complete ordered cycle %q", comment, wantPath)
	}
	if strings.Contains(comment, "cycle members omitted") {
		t.Fatalf("comment = %q, did not want member truncation", comment)
	}
}

func TestCommentsNameEveryAffectedIssue(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	dependencies := make(map[string][]string)
	const nodes = 12
	for i := 0; i < nodes; i++ {
		itemID := fmt.Sprintf("%d", 500+i)
		for j := 0; j < nodes; j++ {
			dependencies[itemID] = append(dependencies[itemID], fmt.Sprintf("%d", 500+j))
		}
	}

	cycle := Find(testRecords(repo, dependencies), Node{Repository: repo, ItemID: "500"})
	if len(cycle.Paths) != MaxPaths || !cycle.MorePaths {
		t.Fatalf("cycle paths = %v, more = %v; want capped dense report", cycle.Paths, cycle.MorePaths)
	}
	comments := Comments(cycle)
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want dense 12-member cycle to fit in one report", len(comments))
	}
	for _, item := range cycle.Affected {
		if !strings.Contains(comments[0], "#"+item.ItemID) {
			t.Errorf("comment = %q, want affected issue #%s", comments[0], item.ItemID)
		}
	}
}

func TestCommentsSplitCompleteMemberList(t *testing.T) {
	cycle := Result{
		Paths:     [][]string{{"10000", "10001", "10000"}},
		MorePaths: true,
	}
	for i := 0; i < 500; i++ {
		cycle.Affected = append(cycle.Affected, Node{ItemID: fmt.Sprintf("%d", 10000+i)})
	}

	comments := Comments(cycle)
	if len(comments) < 3 {
		t.Fatalf("comments = %d, want primary report plus member follow-ups", len(comments))
	}
	combined := strings.Join(comments, "\n")
	for _, comment := range comments {
		if len(comment) > MaxCommentLength {
			t.Errorf("comment length = %d, want at most %d", len(comment), MaxCommentLength)
		}
	}
	for _, item := range cycle.Affected {
		if !strings.Contains(combined, "#"+item.ItemID) {
			t.Errorf("comments omitted affected issue #%s", item.ItemID)
		}
	}
}

func TestCommentsNameUnverifiedMembersOnlyWhenThereAreSome(t *testing.T) {
	cycle := Result{
		Affected: []Node{{ItemID: "1"}, {ItemID: "2"}},
		Paths:    [][]string{{"1", "2", "1"}},
	}
	base := Comments(cycle)
	for _, c := range base {
		if strings.Contains(c, "could not be checked") {
			t.Fatalf("a fully verified cycle carries an unverified note: %q", c)
		}
	}
	cycle.Unverified = []string{"#2: unverifiable: rate limited"}
	withNote := Comments(cycle)
	if len(withNote) != len(base)+1 {
		t.Fatalf("comments %d -> %d, want exactly one more", len(base), len(withNote))
	}
	last := withNote[len(withNote)-1]
	if !strings.Contains(last, "#2: unverifiable: rate limited") || !strings.Contains(last, "needs-human") {
		t.Fatalf("note = %q", last)
	}
	cycle.Unverified = []string{strings.Repeat("x", 3*MaxCommentLength)}
	if long := Comments(cycle); len(long[len(long)-1]) > MaxCommentLength+4 {
		t.Fatalf("note not bounded: %d", len(long[len(long)-1]))
	}
}
