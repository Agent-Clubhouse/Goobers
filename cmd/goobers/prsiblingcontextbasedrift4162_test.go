package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/providers"
)

type fakeBaseDriftProvider struct {
	tip       string
	tipErr    error
	compared  providers.CompareResult
	compErr   error
	comparing []string
}

func (f *fakeBaseDriftProvider) BranchTipSHA(_ context.Context, _ providers.RepositoryRef, _ string) (string, error) {
	return f.tip, f.tipErr
}

func (f *fakeBaseDriftProvider) CompareCommits(_ context.Context, _ providers.RepositoryRef, base, head string) (providers.CompareResult, error) {
	f.comparing = []string{base, head}
	return f.compared, f.compErr
}

// TestResolveBaseDrift is #4162: drift is measured as "is this PR's merge-base
// still the live tip of its base branch", never as a base SHA compared against
// itself.
func TestResolveBaseDrift(t *testing.T) {
	repo := providers.RepositoryRef{Owner: "o", Name: "r"}

	t.Run("merge base equal to the live tip is current", func(t *testing.T) {
		p := &fakeBaseDriftProvider{tip: "tip", compared: providers.CompareResult{MergeBaseSHA: "tip"}}
		got, err := resolveBaseDrift(context.Background(), p, repo, "main", "head")
		if err != nil {
			t.Fatalf("resolveBaseDrift returned error: %v", err)
		}
		if got.Verdict != baseDriftCurrent || got.MergeBaseSHA != "tip" || got.BaseTipSHA != "tip" {
			t.Fatalf("drift = %+v, want current with tip evidence", got)
		}
		if want := []string{"tip", "head"}; len(p.comparing) != 2 || p.comparing[0] != want[0] || p.comparing[1] != want[1] {
			t.Fatalf("compared %v, want the LIVE base tip against the PR head %v", p.comparing, want)
		}
	})

	t.Run("merge base behind the live tip is behind", func(t *testing.T) {
		p := &fakeBaseDriftProvider{tip: "tip", compared: providers.CompareResult{MergeBaseSHA: "older"}}
		got, err := resolveBaseDrift(context.Background(), p, repo, "main", "head")
		if err != nil {
			t.Fatalf("resolveBaseDrift returned error: %v", err)
		}
		if got.Verdict != baseDriftBehind || got.MergeBaseSHA != "older" || got.BaseTipSHA != "tip" {
			t.Fatalf("drift = %+v, want behind with merge-base/tip evidence", got)
		}
	})

	t.Run("a failed tip lookup is unknown, never current", func(t *testing.T) {
		p := &fakeBaseDriftProvider{tipErr: errors.New("boom")}
		got, err := resolveBaseDrift(context.Background(), p, repo, "main", "head")
		if err == nil {
			t.Fatal("resolveBaseDrift: want an error when the base tip cannot be resolved")
		}
		if got.Verdict != baseDriftUnknown {
			t.Fatalf("verdict = %q, want %q", got.Verdict, baseDriftUnknown)
		}
	})

	t.Run("a failed compare is unknown, never current", func(t *testing.T) {
		p := &fakeBaseDriftProvider{tip: "tip", compErr: errors.New("boom")}
		got, err := resolveBaseDrift(context.Background(), p, repo, "main", "head")
		if err == nil {
			t.Fatal("resolveBaseDrift: want an error when the comparison fails")
		}
		if got.Verdict != baseDriftUnknown || got.BaseTipSHA != "tip" {
			t.Fatalf("drift = %+v, want unknown carrying the resolved tip", got)
		}
	})

	t.Run("an empty merge base is unknown, never current", func(t *testing.T) {
		p := &fakeBaseDriftProvider{tip: "tip"}
		got, err := resolveBaseDrift(context.Background(), p, repo, "main", "head")
		if err == nil {
			t.Fatal("resolveBaseDrift: want an error when no merge base is reported")
		}
		if got.Verdict != baseDriftUnknown {
			t.Fatalf("verdict = %q, want %q", got.Verdict, baseDriftUnknown)
		}
	})

	t.Run("missing identity is unknown", func(t *testing.T) {
		p := &fakeBaseDriftProvider{tip: "tip", compared: providers.CompareResult{MergeBaseSHA: "tip"}}
		if got, err := resolveBaseDrift(context.Background(), p, repo, "main", ""); err == nil || got.Verdict != baseDriftUnknown {
			t.Fatalf("drift = %+v, err = %v; want unknown + error without a head SHA", got, err)
		}
	})
}

// gatherSiblingContextBaseDrift runs the stage against a fake GitHub server and
// returns the published drift evidence.
func gatherSiblingContextBaseDrift(t *testing.T, seed func(*fakeGitHubServer)) (drift, mergeBase, tip string) {
	t.Helper()
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(40, "Selected PR")
	server.addOpenPR(40, "goobers/implementation/run-40", "main", "head40", "pinned-base",
		false, nil, []fakePRFile{{path: "a.go", status: "modified"}})
	seed(server)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "40")
	dir := t.TempDir()
	t.Chdir(dir)
	if code, stdout, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sibling-context.json"))
	if err != nil {
		t.Fatalf("read sibling-context.json: %v", err)
	}
	var out struct {
		BaseDrift    string `json:"selectedBaseDrift"`
		MergeBaseSHA string `json:"selectedMergeBaseSha"`
		BaseTipSHA   string `json:"selectedBaseTipSha"`
		BaseSHA      string `json:"selectedBaseSha"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal sibling-context.json: %v", err)
	}
	if out.BaseSHA != "pinned-base" {
		t.Fatalf("selectedBaseSha = %q, want the provider base.sha left untouched (merge-pr's SHA-pin)", out.BaseSHA)
	}
	return out.BaseDrift, out.MergeBaseSHA, out.BaseTipSHA
}

// TestGatherSiblingContextPublishesBaseDrift is #4162: the reviewer receives a
// deterministic base-drift verdict instead of being asked to infer one from a
// base SHA that cannot express it.
func TestGatherSiblingContextPublishesBaseDrift(t *testing.T) {
	t.Run("behind base", func(t *testing.T) {
		drift, mergeBase, tip := gatherSiblingContextBaseDrift(t, func(s *fakeGitHubServer) {
			s.setBranchTip("main", "live-tip")
			s.compares["live-tip...head40"] = fakeCompare{mergeBaseSHA: "older-merge-base"}
		})
		if drift != baseDriftBehind || mergeBase != "older-merge-base" || tip != "live-tip" {
			t.Fatalf("drift = %q, mergeBase = %q, tip = %q; want behind with evidence", drift, mergeBase, tip)
		}
	})

	t.Run("current base", func(t *testing.T) {
		drift, mergeBase, tip := gatherSiblingContextBaseDrift(t, func(s *fakeGitHubServer) {
			s.setBranchTip("main", "live-tip")
			s.compares["live-tip...head40"] = fakeCompare{mergeBaseSHA: "live-tip"}
		})
		if drift != baseDriftCurrent || mergeBase != "live-tip" || tip != "live-tip" {
			t.Fatalf("drift = %q, mergeBase = %q, tip = %q; want current with evidence", drift, mergeBase, tip)
		}
	})

	t.Run("an unresolvable base degrades to unknown without failing the review", func(t *testing.T) {
		drift, mergeBase, tip := gatherSiblingContextBaseDrift(t, func(*fakeGitHubServer) {})
		if drift != baseDriftUnknown || mergeBase != "" || tip != "" {
			t.Fatalf("drift = %q, mergeBase = %q, tip = %q; want unknown with no evidence", drift, mergeBase, tip)
		}
	})
}
