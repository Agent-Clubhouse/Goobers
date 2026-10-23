package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/runsemantics"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

func TestReviewPodConsumesPreparedEvidenceWithoutRecordingAgain(t *testing.T) {
	origin := initBareOrigin(t)
	endpoint, _ := fakeBlobPlane(t)
	previous := checkoutCloneURL
	checkoutCloneURL = func(apiv1.RepoRef) (string, error) { return origin, nil }
	t.Cleanup(func() { checkoutCloneURL = previous })
	t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
	t.Setenv(executor.BaseBranchEnvVar, "main")
	implement := filepath.Join(t.TempDir(), "implement")
	runGitT(t, filepath.Dir(implement), "clone", "--branch", "main", origin, implement)
	delta, _ := publishDeltaFrom(t, implement, "e2e/gate-probe/run-gate", "carried.txt", "checkout bytes must not replace prepared evidence\n")
	planes := newPodPlanes(t, "checkout-token")
	for key, value := range map[string]string{
		dispatcher.EnvDaemonAPI: planes.url, dispatcher.EnvRunID: "run-gate", dispatcher.EnvStage: "review", dispatcher.EnvAttempt: "1", dispatcher.EnvWorkflow: "gate-probe", dispatcher.EnvGaggle: "e2e", executor.RepoProviderEnvVar: string(apiv1.ProviderGitHub), executor.RepoOwnerEnvVar: "acme", executor.RepoNameEnvVar: "widget", executor.BranchNamespaceEnvVar: "e2e/", dispatcher.EnvWorkspaceDelta: delta, dispatcher.EnvStageCapabilities: "", dispatcher.EnvCheckoutCapability: "repo:push",
	} {
		t.Setenv(key, value)
	}
	t.Chdir(t.TempDir())
	prepared := []byte("+prepared-and-scrubbed-evidence\n")
	pointer := seedBlobPlane(t, endpoint, "review.diff", prepared)
	pointer.Integrity = apiv1.IntegrityDerived
	pointer.Artifact.MediaType = runsemantics.ReviewerDiffMediaType
	envelope := apiv1.InvocationEnvelope{RunID: "run-gate", TaskID: "run-gate:review", Goober: "coder", Goal: "gate: review", ContextPointers: []apiv1.ContextPointer{pointer}, PreparedReviewerDiff: &apiv1.PreparedReviewerDiff{Observed: true, Artifact: pointer.Artifact}}
	reviews := 0
	installFakeHarness(t, func(_ context.Context, req harness.RunRequest) error {
		reviews++
		ptr, err := runsemantics.PreparedReviewerDiffPointer(req.Envelope)
		if err != nil || ptr == nil || ptr.Artifact.Digest != pointer.Artifact.Digest {
			return fmt.Errorf("prepared pointer changed: %w", err)
		}
		matches, err := filepath.Glob(filepath.Join(req.Workspace, ".goobers", "context", "*review.diff"))
		if err != nil || len(matches) != 1 {
			return fmt.Errorf("prepared context: %v %w", matches, err)
		}
		data, err := os.ReadFile(matches[0])
		if err != nil {
			return err
		}
		if string(data) != string(prepared) {
			return fmt.Errorf("review consumed %q", data)
		}
		return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.Verdict{Decision: apiv1.VerdictPass})
	})
	publishReviewKit(t, endpoint, envelope, agentickit.ModeReview)
	var stderr strings.Builder
	out := runAgenticStage(t.Context(), &strings.Builder{}, &stderr)
	if out.Result.Status != apiv1.ResultSuccess || out.Verdict == nil || out.Verdict.Decision != apiv1.VerdictPass || reviews != 1 {
		t.Fatalf("outcome=%+v verdict=%+v reviews=%d stderr=%s", out.Result, out.Verdict, reviews, stderr.String())
	}
	planes.mu.Lock()
	defer planes.mu.Unlock()
	for _, body := range planes.journalEmits {
		if strings.Contains(string(body), "reviewer-diff.patch") || strings.Contains(string(body), runsemantics.ReviewerDiffRedactionKind) {
			t.Fatalf("pod duplicated prepared journal evidence: %s", body)
		}
	}
	if strings.Contains(stderr.String(), "reviewer diff: recorded") {
		t.Fatal("pod recaptured diff")
	}
}
