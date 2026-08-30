package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runner"
)

// dispatchreviewredaction_test.go is the pod half of the #3135 correlation
// regression: the local runner journals reviewer-diff-redacted when scrubbing
// transforms the diff a review gate reads, and the pod path — which computes
// the same evidence for a gate running in a pod — owes the same record, in the
// same run journal, or a reviewer finding about redacted content cannot be told
// apart from one about the branch's authoritative raw diff.

// podReviewerDiffJournalPlane stands up a real livejournal.Writer behind the
// pod's own HTTP emit route (the minimal stand-in for
// registerJournalPlaneRoutes that TestPodArtifactRecorderAppendStampsOpTime
// uses), pre-creates the run, and points the pod's environment at it. It
// returns the run's on-disk directory.
func podReviewerDiffJournalPlane(t *testing.T, runID string) string {
	t.Helper()
	runsDir := filepath.Join(t.TempDir(), "runs")
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "review-redaction", WorkflowVersion: 1, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close run: %v", err)
	}

	writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) {
		if gaggle != "goobers" {
			return "", false
		}
		return runsDir, true
	})
	if err != nil {
		t.Fatalf("livejournal.NewWriter: %v", err)
	}
	t.Cleanup(writer.Close)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req livejournal.EmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp, err := writer.Emit(r.Context(), req)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(server.Close)

	t.Setenv(dispatcher.EnvDaemonAPI, server.URL)
	t.Setenv(dispatcher.EnvRunID, runID)
	t.Setenv(dispatcher.EnvGaggle, "goobers")
	t.Setenv(dispatcher.EnvStage, "review")
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	return filepath.Join(runsDir, runID)
}

// podReviewerDiffWorkspace clones a fresh origin, branches off main, and
// commits body as changed.txt — the shape recordPodReviewerDiff diffs.
func podReviewerDiffWorkspace(t *testing.T, body string) string {
	t.Helper()
	origin := initBareOrigin(t)
	ws := filepath.Join(t.TempDir(), "ws")
	runGitT(t, filepath.Dir(ws), "clone", "--branch", "main", origin, ws)
	runGitT(t, ws, "checkout", "-b", "e2e/wf/run-redaction")
	runGitT(t, ws, "config", "user.name", "t")
	runGitT(t, ws, "config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(ws, "changed.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, ws, "add", "changed.txt")
	runGitT(t, ws, "commit", "-q", "-m", "change")
	return ws
}

func podReviewerDiffAnnotations(t *testing.T, dir string) []journal.Event {
	t.Helper()
	rd, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var out []journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventRunnerAnnotation && ev.Name == runner.ReviewerDiffRedactionAnnotation {
			out = append(out, ev)
		}
	}
	return out
}

// TestPodReviewerDiffRedactionRecorded mirrors the runner's
// TestReviewerDiffRedactionRecorded on the pod/cloud path: a minted credential
// captured in a commit is scrubbed out of the evidence, and the run journal
// carries both digests so the evidence stays correlatable with the raw diff.
func TestPodReviewerDiffRedactionRecorded(t *testing.T) {
	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
	t.Setenv(executor.BaseBranchEnvVar, "main")
	dir := podReviewerDiffJournalPlane(t, "run-pod-reviewer-diff-redacted")
	ws := podReviewerDiffWorkspace(t, "secret-value-0123456789abcdef\n")

	var stderr strings.Builder
	creds := []dispatcher.MintedCredential{{Capability: "repo:push", Value: "secret-value-0123456789abcdef"}}
	ptr, err := recordPodReviewerDiff(context.Background(), ws, t.TempDir(), "review", creds, &stderr)
	if err != nil || ptr == nil || ptr.Artifact == nil {
		t.Fatalf("pointer %+v err %v, want the diff evidence", ptr, err)
	}

	hits := podReviewerDiffAnnotations(t, dir)
	if len(hits) != 1 {
		t.Fatalf("reviewer-diff-redacted events = %d, want 1\nstderr:\n%s", len(hits), stderr.String())
	}
	ev := hits[0]
	if ev.Stage != "review" {
		t.Fatalf("stage = %q, want review", ev.Stage)
	}
	if got := ev.Runner["evidenceDigest"]; got != ptr.Artifact.Digest {
		t.Fatalf("evidenceDigest = %v, want the pointer's digest %s", got, ptr.Artifact.Digest)
	}
	raw, _ := ev.Runner["rawDigest"].(string)
	if raw == "" || raw == ptr.Artifact.Digest {
		t.Fatalf("rawDigest = %q, want the pre-scrub digest, distinct from the evidence digest", raw)
	}
	if got := ev.Runner["rawBytes"]; got == nil || got == float64(0) {
		t.Fatalf("rawBytes = %v, want the pre-scrub byte count", got)
	}
	if !strings.Contains(stderr.String(), "redacted before review: raw "+raw) {
		t.Errorf("pod stderr does not report the raw digest beside the evidence digest:\n%s", stderr.String())
	}
}

// TestPodReviewerDiffRedactionSilentWhenUntransformed keeps the annotation
// meaningful on the pod path too: evidence byte-identical to the raw diff
// records nothing.
func TestPodReviewerDiffRedactionSilentWhenUntransformed(t *testing.T) {
	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
	t.Setenv(executor.BaseBranchEnvVar, "main")
	dir := podReviewerDiffJournalPlane(t, "run-pod-reviewer-diff-clean")
	ws := podReviewerDiffWorkspace(t, "req.Header.Set(\"Authorization\", \"Bearer \"+token)\n")

	var stderr strings.Builder
	ptr, err := recordPodReviewerDiff(context.Background(), ws, t.TempDir(), "review", nil, &stderr)
	if err != nil || ptr == nil {
		t.Fatalf("pointer %+v err %v, want the diff evidence", ptr, err)
	}
	if hits := podReviewerDiffAnnotations(t, dir); len(hits) != 0 {
		t.Fatalf("reviewer-diff-redacted events = %d, want 0 for an untransformed diff", len(hits))
	}
	if strings.Contains(stderr.String(), "redacted before review") {
		t.Errorf("pod stderr claims a redaction that did not happen:\n%s", stderr.String())
	}
}
