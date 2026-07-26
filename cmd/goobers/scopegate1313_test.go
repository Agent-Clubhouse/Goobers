package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestGatherSiblingContextEmitsSelectedChangedLines is #1313's foundation:
// the scope gate's second magnitude (total changed lines, additions +
// deletions summed across every file) must be computed and emitted
// alongside the existing #1111 selectedChangedFiles count, from the same
// already-fetched PullRequestFiles data (no extra provider call).
func TestGatherSiblingContextEmitsSelectedChangedLines(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(20, "Selected PR")
	server.addOpenPR(20, "goobers/implementation/run-20", "main", "sha20", "base", false, nil, []fakePRFile{
		{path: "a.go", status: "modified", additions: 120, deletions: 30},
		{path: "b.go", status: "modified", additions: 5, deletions: 2},
	})

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1313")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "20")
	dir := t.TempDir()
	t.Chdir(dir)

	if code, _, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stderr = %q", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sibling-context.json"))
	if err != nil {
		t.Fatalf("read sibling-context.json: %v", err)
	}
	var out struct {
		SelectedChangedFiles string `json:"selectedChangedFiles"`
		SelectedChangedLines string `json:"selectedChangedLines"`
		ScopeGateParked      string `json:"scopeGateParked"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.SelectedChangedFiles != "2" {
		t.Errorf("selectedChangedFiles = %q, want %q", out.SelectedChangedFiles, "2")
	}
	if out.SelectedChangedLines != "157" {
		t.Errorf("selectedChangedLines = %q, want %q (120+30+5+2)", out.SelectedChangedLines, "157")
	}
	if out.ScopeGateParked != "false" {
		t.Errorf("scopeGateParked = %q, want %q (small diff, far under default thresholds)", out.ScopeGateParked, "false")
	}
}

// TestGatherSiblingContextParksOversizedPR is #1313's end-to-end integration
// path: a PR whose changed-file count meets the gate's threshold gets parked
// by gather-sibling-context itself (label + comment), and reports
// scopeGateParked="true" so merge-review's scope-gate branches away from
// review this cycle.
func TestGatherSiblingContextParksOversizedPR(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(30, "Oversized PR")
	files := make([]fakePRFile, 60)
	for i := range files {
		files[i] = fakePRFile{path: "file" + string(rune('a'+i%26)) + ".go", status: "modified", additions: 1}
	}
	server.addOpenPR(30, "goobers/implementation/run-30", "main", "sha30", "base", false, nil, files)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1313-parked")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "30")
	dir := t.TempDir()
	t.Chdir(dir)

	if code, _, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stderr = %q", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sibling-context.json"))
	if err != nil {
		t.Fatalf("read sibling-context.json: %v", err)
	}
	var out struct {
		ScopeGateParked string `json:"scopeGateParked"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ScopeGateParked != "true" {
		t.Fatalf("scopeGateParked = %q, want %q (60 files, over the default 50-file threshold)", out.ScopeGateParked, "true")
	}
	if !issueHasLabel(server, 30, scopeGateLabel) {
		t.Fatal("expected goobers:scope-gate to be applied")
	}
}

// TestReconcileScopeGate covers #1313's full park/self-heal/ack lifecycle:
// parking a fresh over-threshold PR (either dimension trips it), staying
// idempotent while already parked, clearing on shrink, clearing on an
// operator ack even while still over threshold, and never touching a PR
// that was never over threshold — mirroring TestFlagScopeDrift's table
// shape for the advisory flag this gate escalates.
func TestReconcileScopeGate(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

	t.Run("over files threshold, unlabeled -> parks + comments once", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(10, "big pr")
		provider := server.newGitHubProvider("token")
		parked, changed, err := reconcileScopeGate(context.Background(), provider, repo, 10, nil, 73, 500, 50, 2000)
		if err != nil {
			t.Fatalf("reconcileScopeGate: %v", err)
		}
		if !parked {
			t.Fatal("parked = false, want true (over the files threshold)")
		}
		if !changed {
			t.Fatal("changed = false, want true — should have applied the label")
		}
		if !issueHasLabel(server, 10, scopeGateLabel) {
			t.Fatal("expected goobers:scope-gate to be applied")
		}
		if got := issueCommentCount(server, 10); got != 1 {
			t.Fatalf("comment count = %d, want exactly 1", got)
		}
	})

	t.Run("over lines threshold only, unlabeled -> parks too (whichever trips first)", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(11, "big pr")
		provider := server.newGitHubProvider("token")
		parked, changed, err := reconcileScopeGate(context.Background(), provider, repo, 11, nil, 5, 3000, 50, 2000)
		if err != nil {
			t.Fatalf("reconcileScopeGate: %v", err)
		}
		if !parked || !changed {
			t.Fatalf("parked=%v changed=%v, want true/true (over the lines threshold alone)", parked, changed)
		}
	})

	t.Run("over threshold, already labeled -> idempotent no-op", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(12, "big pr")
		provider := server.newGitHubProvider("token")
		parked, changed, err := reconcileScopeGate(context.Background(), provider, repo, 12, []string{scopeGateLabel}, 73, 500, 50, 2000)
		if err != nil {
			t.Fatalf("reconcileScopeGate: %v", err)
		}
		if !parked {
			t.Fatal("parked = false, want true")
		}
		if changed {
			t.Fatal("changed = true, want false — already parked, must not re-comment")
		}
		if got := issueCommentCount(server, 12); got != 0 {
			t.Fatalf("comment count = %d, want 0 (no re-comment on an already-parked PR)", got)
		}
	})

	t.Run("shrunk back under both thresholds, labeled -> clears and comments", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(13, "shrunk pr")
		provider := server.newGitHubProvider("token")
		parked, changed, err := reconcileScopeGate(context.Background(), provider, repo, 13, []string{scopeGateLabel}, 5, 100, 50, 2000)
		if err != nil {
			t.Fatalf("reconcileScopeGate: %v", err)
		}
		if parked {
			t.Fatal("parked = true, want false — shrunk back under both thresholds")
		}
		if !changed {
			t.Fatal("changed = false, want true — should have cleared the label")
		}
		if issueHasLabel(server, 13, scopeGateLabel) {
			t.Fatal("expected goobers:scope-gate to be cleared")
		}
		if got := issueCommentCount(server, 13); got != 1 {
			t.Fatalf("comment count = %d, want exactly 1 (release comment)", got)
		}
	})

	t.Run("operator ack releases it even while still over threshold", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(14, "acked pr")
		provider := server.newGitHubProvider("token")
		parked, changed, err := reconcileScopeGate(context.Background(), provider, repo, 14,
			[]string{scopeGateLabel, scopeGateAckLabel}, 200, 5000, 50, 2000)
		if err != nil {
			t.Fatalf("reconcileScopeGate: %v", err)
		}
		if parked {
			t.Fatal("parked = true, want false — an operator ack releases it despite the size")
		}
		if !changed {
			t.Fatal("changed = false, want true — should have cleared the label")
		}
	})

	t.Run("under threshold, unlabeled -> no-op", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(15, "small pr")
		provider := server.newGitHubProvider("token")
		parked, changed, err := reconcileScopeGate(context.Background(), provider, repo, 15, nil, 3, 40, 50, 2000)
		if err != nil {
			t.Fatalf("reconcileScopeGate: %v", err)
		}
		if parked || changed {
			t.Fatalf("parked=%v changed=%v, want false/false — never over threshold", parked, changed)
		}
		if got := issueCommentCount(server, 15); got != 0 {
			t.Fatalf("comment count = %d, want 0", got)
		}
	})

	t.Run("threshold <= 0 disables that dimension", func(t *testing.T) {
		server := newFakeGitHubServer(t, repo.Owner, repo.Name)
		server.addIssue(16, "huge but disabled pr")
		provider := server.newGitHubProvider("token")
		parked, _, err := reconcileScopeGate(context.Background(), provider, repo, 16, nil, 10000, 1000000, 0, 0)
		if err != nil {
			t.Fatalf("reconcileScopeGate: %v", err)
		}
		if parked {
			t.Fatal("parked = true, want false — both thresholds disabled")
		}
	})
}
