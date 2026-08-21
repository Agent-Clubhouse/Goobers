package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

// seedUnpushedDiffRun creates a terminal-looking prior run journal carrying
// the #3366 unpushed-diff artifact pair the runner records: the diff bytes and
// the discovery sidecar naming the item(s) the run was working on. pushed
// additionally journals the ref.touched branch-push event push-branch's
// mutation sidecar produces, marking the work as published (not stranded).
func seedUnpushedDiffRun(t *testing.T, root, gaggle, runID string, at time.Time, itemIDs []string, diff string, pushed bool) {
	t.Helper()
	run, err := journal.Create(layoutFor(root).ForGaggle(gaggle).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", Gaggle: gaggle,
	}, nil, journal.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("create prior run: %v", err)
	}
	patchRef, err := run.RecordStageArtifact("implement", 1, "", "implement/unpushed-diff.patch", []byte(diff))
	if err != nil {
		t.Fatalf("record prior diff artifact: %v", err)
	}
	meta := map[string]interface{}{
		"schema":     "goobers.dev/unpushed-diff/v1",
		"runId":      runID,
		"workflow":   "implementation",
		"stage":      "implement",
		"attempt":    1,
		"itemIds":    itemIDs,
		"branch":     "goobers/implementation/" + runID,
		"baseRef":    "main",
		"recordedAt": at,
		"diffBytes":  len(diff),
		"diff": map[string]interface{}{
			"path": patchRef.Path, "digest": patchRef.Digest, "size": patchRef.Size,
		},
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal prior metadata: %v", err)
	}
	if _, err := run.RecordStageArtifact("implement", 1, "", "implement/unpushed-diff.json", metaJSON); err != nil {
		t.Fatalf("record prior metadata artifact: %v", err)
	}
	if pushed {
		if err := run.Append(journal.Event{
			Type: journal.EventRefTouched, Stage: "push-branch", Attempt: 1,
			ExternalRef: &journal.ExternalRef{Provider: "git", Kind: "branch", ID: "goobers/implementation/" + runID},
			Runner:      map[string]any{"operation": "push"},
		}); err != nil {
			t.Fatalf("append push event: %v", err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close prior run: %v", err)
	}
}

// TestGatherImplementContextOffersPriorUnpushedDiff is #3366's re-claim
// discovery: a new run that claims the same backlog item a prior run stranded
// work on gets that diff offered in its implementation context — resume
// instead of redo. A prior run that PUBLISHED its work (pushed branch) and a
// prior run on a different item are both excluded.
func TestGatherImplementContextOffersPriorUnpushedDiff(t *testing.T) {
	root := initDemo(t)
	now := time.Now().UTC()
	const gaggle = "acme-web"
	const currentRunID = "run-gather-3366"

	// The stranded run: committed work for item 42, never published. An older
	// stranded run for the same item proves "newest wins".
	seedUnpushedDiffRun(t, root, gaggle, "stranded-run", now.Add(-2*time.Hour), []string{"42"},
		"diff --git a/impl.txt b/impl.txt\n+the stranded validated work\n", false)
	seedUnpushedDiffRun(t, root, gaggle, "older-stranded-run", now.Add(-20*time.Hour), []string{"42"},
		"diff --git a/impl.txt b/impl.txt\n+older stranded work\n", false)
	// Published: same item, but its journal shows the branch was pushed.
	seedUnpushedDiffRun(t, root, gaggle, "published-run", now.Add(-1*time.Hour), []string{"42"},
		"diff --git a/impl.txt b/impl.txt\n+published work\n", true)
	// Different item entirely.
	seedUnpushedDiffRun(t, root, gaggle, "other-item-run", now.Add(-1*time.Hour), []string{"99"},
		"diff --git a/other.txt b/other.txt\n+unrelated work\n", false)

	// The current run claims item 42, exactly as query-backlog would have
	// before this stage runs.
	schedulerDir := layoutFor(root).SchedulerDir()
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		t.Fatalf("create scheduler dir: %v", err)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if ok, holder, err := ledger.Claim("42", currentRunID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("claim item 42: ok=%v holder=%q err=%v", ok, holder, err)
	}

	server := newFakeGitHubServer(t, "your-org", "your-repo")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", currentRunID)
	t.Setenv("GOOBERS_GAGGLE", gaggle)

	workDir := t.TempDir()
	t.Chdir(workDir)
	if code, stdout, stderr := runArgs(t, "gather-implement-context", root); code != 0 {
		t.Fatalf("gather-implement-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	data, err := os.ReadFile(filepath.Join(workDir, implementationContextResultFile))
	if err != nil {
		t.Fatalf("read implementation context: %v", err)
	}
	var got implementationContext
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal implementation context: %v", err)
	}
	prior := got.PriorUnpushedWork
	if prior == nil {
		t.Fatal("priorUnpushedWork missing — the stranded diff is not discoverable on re-claim (#3366)")
	}
	if prior.RunID != "stranded-run" {
		t.Fatalf("priorUnpushedWork.runId = %q, want stranded-run (newest stranded, published and other-item runs excluded)", prior.RunID)
	}
	if !strings.Contains(prior.Diff, "the stranded validated work") {
		t.Fatalf("priorUnpushedWork.diff = %q, want the stranded diff inline", prior.Diff)
	}
	if prior.Stage != "implement" || prior.BaseRef != "main" ||
		prior.Branch != "goobers/implementation/stranded-run" || prior.DiffDigest == "" {
		t.Fatalf("priorUnpushedWork = %+v, want implement/main/branch/digest populated", prior)
	}
	if prior.Note == "" {
		t.Fatal("priorUnpushedWork.note empty — the consumer needs the staleness caveat")
	}
}

// TestPriorUnpushedWorkFromRunRespectsPublicationOrder exercises the
// per-run ordering decision directly, without the claim-ledger plumbing.
func TestPriorUnpushedWorkFromRunRespectsPublicationOrder(t *testing.T) {
	root := initDemo(t)
	now := time.Now().UTC()
	const gaggle = "acme-web"
	runsDir := layoutFor(root).ForGaggle(gaggle).RunsDir()

	// Publication BEFORE the diff: still stranded, must be offered.
	seedOrderedRun(t, runsDir, "push-then-strand", now, true, false)
	if got := priorUnpushedWorkFromRun(filepath.Join(runsDir, "push-then-strand"),
		[]string{"42"}, now.Add(-24*time.Hour), io.Discard); got == nil {
		t.Fatal("a diff recorded AFTER the branch was pushed is still unpublished — it must be offered")
	}

	// Publication AFTER the diff: published, must be excluded.
	seedOrderedRun(t, runsDir, "strand-then-push", now, false, true)
	if got := priorUnpushedWorkFromRun(filepath.Join(runsDir, "strand-then-push"),
		[]string{"42"}, now.Add(-24*time.Hour), io.Discard); got != nil {
		t.Fatalf("priorUnpushedWorkFromRun = %+v, want nil — the work was published after the diff", got)
	}
}

// seedOrderedRun writes a run journal with a branch-push ref.touched event
// before and/or after the unpushed-diff artifact pair.
func seedOrderedRun(t *testing.T, runsDir, runID string, at time.Time, pushBefore, pushAfter bool) {
	t.Helper()
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "implementation", Gaggle: "acme-web",
	}, nil, journal.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("create %s: %v", runID, err)
	}
	push := func() {
		if err := run.Append(journal.Event{
			Type:        journal.EventRefTouched,
			ExternalRef: &journal.ExternalRef{Provider: "git", Kind: "branch", ID: "goobers/implementation/" + runID},
		}); err != nil {
			t.Fatalf("append push event: %v", err)
		}
	}
	if pushBefore {
		push()
	}
	const diff = "diff --git a/impl.txt b/impl.txt\n+work\n"
	patchRef, err := run.RecordStageArtifact("implement", 1, "", "implement/unpushed-diff.patch", []byte(diff))
	if err != nil {
		t.Fatalf("record diff: %v", err)
	}
	metaJSON, err := json.Marshal(map[string]interface{}{
		"schema": "goobers.dev/unpushed-diff/v1", "runId": runID, "stage": "implement",
		"attempt": 1, "itemIds": []string{"42"}, "baseRef": "main", "recordedAt": at,
		"diffBytes": len(diff),
		"diff":      map[string]interface{}{"path": patchRef.Path, "digest": patchRef.Digest, "size": patchRef.Size},
	})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if _, err := run.RecordStageArtifact("implement", 1, "", "implement/unpushed-diff.json", metaJSON); err != nil {
		t.Fatalf("record metadata: %v", err)
	}
	if pushAfter {
		push()
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close %s: %v", runID, err)
	}
}

// TestGatherImplementContextNoPriorWorkWithoutClaim: a run with no claimed
// item (or none stranded) emits no priorUnpushedWork section — byte-compatible
// with the pre-#3366 context for the common case.
func TestGatherImplementContextNoPriorWorkWithoutClaim(t *testing.T) {
	root := initDemo(t)
	seedUnpushedDiffRun(t, root, "acme-web", "stranded-run", time.Now().UTC().Add(-2*time.Hour), []string{"42"},
		"diff --git a/impl.txt b/impl.txt\n+stranded\n", false)

	server := newFakeGitHubServer(t, "your-org", "your-repo")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-unclaimed")
	t.Setenv("GOOBERS_GAGGLE", "acme-web")

	workDir := t.TempDir()
	t.Chdir(workDir)
	if code, stdout, stderr := runArgs(t, "gather-implement-context", root); code != 0 {
		t.Fatalf("gather-implement-context: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, implementationContextResultFile))
	if err != nil {
		t.Fatalf("read implementation context: %v", err)
	}
	var got implementationContext
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal implementation context: %v", err)
	}
	if got.PriorUnpushedWork != nil {
		t.Fatalf("priorUnpushedWork = %+v, want none for a run with no claim", got.PriorUnpushedWork)
	}
	if strings.Contains(string(data), "priorUnpushedWork") {
		t.Fatalf("context JSON = %s, want the priorUnpushedWork key omitted entirely", data)
	}
}
