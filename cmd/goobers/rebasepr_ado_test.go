package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

// TestRebasePRADOCleanForcePushesAndClearsLabel exercises the rebase-pr ADO
// branch's happy path: the provider-neutral git core (checkout, fetch/rebase, and
// the mandatory force-with-lease) runs verbatim, and on a clean rebase with no
// substantive finding the goobers:needs-remediation marker is cleared through the
// native PR-label DELETE — never UpdateWorkItem(ID: PR#), which on ADO would
// mutate the unrelated work item sharing the PR's numeric id.
func TestRebasePRADOCleanForcePushesAndClearsLabel(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)

	const prBranch = "goobers/impl/ado-clean"
	origin := initNonConflictingPRBranch(t, prBranch)
	wt := prWorktree(t, origin, prBranch)

	st := &adoRemediationServerState{
		owner: repo.Owner, project: repo.Project, name: repo.Name,
		prNumber: 55, prBranch: prBranch, base: "main",
		headSHA: "unused-head", baseSHA: "unused-base", status: "active",
		labels: []string{needsRemediationLabel, "some-other-label"},
	}
	server := st.start(t)
	installADOStageProvider(t, repo, server)

	t.Setenv("GOOBERS_RUN_ID", "run-363-ado")
	t.Setenv("GOOBERS_WORKFLOW", "pr-remediation")
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "test-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", "55")
	t.Setenv("GOOBERS_INPUT_HEAD", prBranch)
	t.Setenv("GOOBERS_INPUT_BASE", "main")
	t.Setenv("GOOBERS_INPUT_HASSUBSTANTIVEFINDINGS", "false")
	t.Chdir(wt.Path)

	code, stdout, stderr := runArgs(t, "rebase-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "clean rebase") {
		t.Fatalf("stdout = %q, want a mention of a clean rebase", stdout)
	}

	// The rebase actually applied: main's commit (unrelated.txt) is now present
	// on the checked-out branch.
	if _, err := os.Stat(filepath.Join(wt.Path, "unrelated.txt")); err != nil {
		t.Fatalf("unrelated.txt (main's commit) missing after rebase: %v", err)
	}

	// The force-push reached origin, not just the local worktree.
	verify := t.TempDir()
	runGitT(t, verify, "clone", "--branch", prBranch, origin, filepath.Join(verify, "check"))
	if _, err := os.Stat(filepath.Join(verify, "check", "unrelated.txt")); err != nil {
		t.Fatalf("origin's %s branch missing the rebased commit after force-push: %v", prBranch, err)
	}

	// The label was cleared via the native PR-label DELETE, and no wit/workitems
	// endpoint was ever touched.
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, l := range st.labels {
		if l == needsRemediationLabel {
			t.Fatalf("labels = %v, want %s cleared", st.labels, needsRemediationLabel)
		}
	}
	if len(st.labels) != 1 || st.labels[0] != "some-other-label" {
		t.Fatalf("labels = %v, want only the untouched other label to remain", st.labels)
	}
	if len(st.deletedLabels) != 1 || st.deletedLabels[0] != needsRemediationLabel {
		t.Fatalf("deletedLabels = %v, want exactly [%s] via the native PR-label DELETE", st.deletedLabels, needsRemediationLabel)
	}
	if st.workItemHit {
		t.Error("a wit/workitems endpoint was called — the PR-as-work-item wrong-object hazard was hit")
	}

	data, err := os.ReadFile(filepath.Join(wt.Path, "rebase-result.json"))
	if err != nil {
		t.Fatalf("read rebase-result.json: %v", err)
	}
	if !strings.Contains(string(data), `"needsAgent":"false"`) {
		t.Fatalf("rebase-result.json = %s, want needsAgent=false", data)
	}
}
