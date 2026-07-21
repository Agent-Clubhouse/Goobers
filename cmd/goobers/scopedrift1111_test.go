package main

import (
	"context"
	"testing"

	"github.com/goobers/goobers/providers"
)

func issueCommentCount(server *fakeGitHubServer, number int) int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return len(server.issues[number].comments)
}

// TestFlagScopeDrift covers #1111's flag lifecycle: a PR over the threshold and
// not yet labeled gets goobers:scope-drift + one explanatory comment; a PR
// already labeled is a no-op (idempotent — never re-comments); a PR that has
// shrunk back under the threshold has the label cleared; and threshold 0
// disables the guard entirely.
func TestFlagScopeDrift(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	t.Run("over threshold, unlabeled -> flags + comments once", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(10, "big pr")
		provider := server.newGitHubProvider("token")
		changed, err := flagScopeDrift(context.Background(), provider, repo, 10, nil, 73, 50)
		if err != nil {
			t.Fatalf("flagScopeDrift: %v", err)
		}
		if !changed {
			t.Fatal("changed = false, want true — should have applied the label")
		}
		if !issueHasLabel(server, 10, scopeDriftLabel) {
			t.Fatal("expected goobers:scope-drift to be applied")
		}
		if got := issueCommentCount(server, 10); got != 1 {
			t.Fatalf("comment count = %d, want exactly 1", got)
		}
	})

	t.Run("over threshold, already labeled -> idempotent no-op", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(11, "big pr")
		provider := server.newGitHubProvider("token")
		changed, err := flagScopeDrift(context.Background(), provider, repo, 11, []string{scopeDriftLabel}, 73, 50)
		if err != nil {
			t.Fatalf("flagScopeDrift: %v", err)
		}
		if changed {
			t.Fatal("changed = true, want false — already flagged, must not re-comment")
		}
		if got := issueCommentCount(server, 11); got != 0 {
			t.Fatalf("comment count = %d, want 0 (no re-comment on an already-flagged PR)", got)
		}
	})

	t.Run("under threshold, labeled -> clears the label", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(12, "shrunk pr")
		server.issues[12].labels = []string{scopeDriftLabel}
		provider := server.newGitHubProvider("token")
		changed, err := flagScopeDrift(context.Background(), provider, repo, 12, []string{scopeDriftLabel}, 8, 50)
		if err != nil {
			t.Fatalf("flagScopeDrift: %v", err)
		}
		if !changed {
			t.Fatal("changed = false, want true — should have cleared the label")
		}
		if issueHasLabel(server, 12, scopeDriftLabel) {
			t.Fatal("expected goobers:scope-drift to be cleared once the PR shrank under threshold")
		}
	})

	t.Run("under threshold, unlabeled -> no-op", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(13, "small pr")
		provider := server.newGitHubProvider("token")
		changed, err := flagScopeDrift(context.Background(), provider, repo, 13, nil, 8, 50)
		if err != nil {
			t.Fatalf("flagScopeDrift: %v", err)
		}
		if changed {
			t.Fatal("changed = true, want false — ordinary PR should not be touched")
		}
	})

	t.Run("threshold 0 disables the guard", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(14, "huge pr")
		provider := server.newGitHubProvider("token")
		changed, err := flagScopeDrift(context.Background(), provider, repo, 14, nil, 999, 0)
		if err != nil {
			t.Fatalf("flagScopeDrift: %v", err)
		}
		if changed || issueHasLabel(server, 14, scopeDriftLabel) {
			t.Fatal("threshold 0 must disable the guard entirely")
		}
	})
}
