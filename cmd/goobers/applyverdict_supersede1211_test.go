package main

import (
	"context"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestSupersededByIdenticalSibling is #1211: two open PRs that implement
// DIFFERENT issues can still converge to a byte-identical tree (the #1179/#1180
// deadlock, which duplicateOfEarlierPR — shared-issue only — misses). The later
// one is superseded and closable, linking the earlier survivor; the earlier one
// is not; and any non-identical or unverifiable diff fails closed (never
// auto-closes), because this action closes a pull request.
func TestSupersededByIdenticalSibling(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)

	// The evidenced case: #1179 (REL-3, issue #432) and #1180 (REL-2, issue #433)
	// converge to the identical two-file tree, byte-for-byte.
	identical := []fakePRFile{
		{path: "release/main.go", status: "modified", additions: 10, deletions: 2, patch: "@@ -1,4 +1,12 @@\n+release engine\n"},
		{path: "release/zip.go", status: "added", additions: 30, deletions: 0, patch: "@@ -0,0 +1,30 @@\n+zip writer\n"},
	}
	// Same files, one patch differs by a byte.
	different := []fakePRFile{
		{path: "release/main.go", status: "modified", additions: 10, deletions: 2, patch: "@@ -1,4 +1,12 @@\n+release engine\n"},
		{path: "release/zip.go", status: "added", additions: 31, deletions: 0, patch: "@@ -0,0 +1,31 @@\n+a DIFFERENT zip writer\n"},
	}
	// Identical paths to #1179 but one patch omitted (binary/over-size): byte
	// identity is unverifiable, so it must fail closed.
	unverifiable := []fakePRFile{
		{path: "release/main.go", status: "modified", additions: 10, deletions: 2, patch: "@@ -1,4 +1,12 @@\n+release engine\n"},
		{path: "release/zip.go", status: "added", additions: 30, deletions: 0, patch: ""},
	}
	// The identical tree, but with the two files reported in the opposite order.
	reversed := []fakePRFile{identical[1], identical[0]}

	server.addOpenPR(1179, "goobers/implementation/rel3", "main", "h1179", "bmain", false, nil, identical)
	server.addOpenPR(1180, "goobers/implementation/rel2", "main", "h1180", "bmain", false, nil, identical)
	server.addOpenPR(1181, "goobers/implementation/other", "main", "h1181", "bmain", false, nil, different)
	server.addOpenPR(1182, "goobers/implementation/bin", "main", "h1182", "bmain", false, nil, unverifiable)
	server.addOpenPR(1183, "goobers/implementation/rel2b", "main", "h1183", "bmain", false, nil, reversed)

	provider := server.newGitHubProvider("token")
	ctx := context.Background()

	t.Run("later byte-identical sibling is superseded by the earlier one", func(t *testing.T) {
		pr := &providers.PullRequestSummary{Number: 1180, Body: "Implements #433", State: "open"}
		reason, superseded := supersededByIdenticalSibling(ctx, provider, repo, pr)
		if !superseded {
			t.Fatalf("superseded = false, want true; reason = %q", reason)
		}
		if !strings.Contains(reason, "#1179") {
			t.Errorf("reason must link the surviving PR #1179 for the close comment; got %q", reason)
		}
	})
	t.Run("the earlier PR of a byte-identical pair is not superseded", func(t *testing.T) {
		pr := &providers.PullRequestSummary{Number: 1179, Body: "Implements #432", State: "open"}
		if _, superseded := supersededByIdenticalSibling(ctx, provider, repo, pr); superseded {
			t.Fatal("the earliest of a byte-identical pair must never be closed — fifo, it wins")
		}
	})
	t.Run("a byte-different sibling is never superseded", func(t *testing.T) {
		pr := &providers.PullRequestSummary{Number: 1181, Body: "Implements #500", State: "open"}
		if _, superseded := supersededByIdenticalSibling(ctx, provider, repo, pr); superseded {
			t.Fatal("a PR with a different diff must not be closed as superseded")
		}
	})
	t.Run("fails closed when a file's patch is unverifiable", func(t *testing.T) {
		pr := &providers.PullRequestSummary{Number: 1182, Body: "Implements #501", State: "open"}
		if _, superseded := supersededByIdenticalSibling(ctx, provider, repo, pr); superseded {
			t.Fatal("an unverifiable (omitted-patch) diff must fail closed, not auto-close")
		}
	})
	t.Run("byte-identity is independent of provider file ordering", func(t *testing.T) {
		pr := &providers.PullRequestSummary{Number: 1183, Body: "Implements #502", State: "open"}
		reason, superseded := supersededByIdenticalSibling(ctx, provider, repo, pr)
		if !superseded {
			t.Fatalf("a reversed-order identical diff must still match #1179; reason = %q", reason)
		}
	})
}

// TestChangedDiffDigest checks the diff-digest primitive directly: identical
// diffs (any file order) hash equal, a one-byte patch change diverges, and the
// unverifiable/empty cases return ok=false so callers fail closed.
func TestChangedDiffDigest(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}
	server := newFakeGitHubServer(t, repo.Owner, repo.Name)

	files := []fakePRFile{
		{path: "b.go", status: "modified", patch: "@@ -1 +1 @@\n-x\n+y\n"},
		{path: "a.go", status: "added", patch: "@@ -0,0 +1 @@\n+new\n"},
	}
	server.addOpenPR(1, "goobers/implementation/a", "main", "h1", "b", false, nil, files)
	server.addOpenPR(2, "goobers/implementation/b", "main", "h2", "b", false, nil,
		[]fakePRFile{files[1], files[0]}) // same two files, reversed
	server.addOpenPR(3, "goobers/implementation/c", "main", "h3", "b", false, nil,
		[]fakePRFile{{path: "a.go", status: "added", patch: "@@ -0,0 +1 @@\n+new\n"}, {path: "b.go", status: "modified", patch: "@@ -1 +1 @@\n-x\n+Y\n"}}) // one byte differs
	server.addOpenPR(4, "goobers/implementation/d", "main", "h4", "b", false, nil,
		[]fakePRFile{{path: "a.go", status: "added", patch: ""}}) // omitted patch
	server.addOpenPR(5, "goobers/implementation/e", "main", "h5", "b", false, nil, nil) // empty diff

	provider := server.newGitHubProvider("token")
	ctx := context.Background()

	d1, ok1 := changedDiffDigest(ctx, provider, repo, 1)
	d2, ok2 := changedDiffDigest(ctx, provider, repo, 2)
	d3, ok3 := changedDiffDigest(ctx, provider, repo, 3)
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("digests 1/2/3 should be computable: ok = %v/%v/%v", ok1, ok2, ok3)
	}
	if d1 != d2 {
		t.Errorf("identical diffs in different file order must hash equal: %q vs %q", d1, d2)
	}
	if d1 == d3 {
		t.Error("a one-byte patch difference must change the digest")
	}
	if _, ok := changedDiffDigest(ctx, provider, repo, 4); ok {
		t.Error("a file with an omitted patch must return ok=false (unverifiable)")
	}
	if _, ok := changedDiffDigest(ctx, provider, repo, 5); ok {
		t.Error("an empty diff must return ok=false (mootFailReason owns that case)")
	}
}
