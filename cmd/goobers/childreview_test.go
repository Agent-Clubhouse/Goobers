package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

func TestChildReviewUsesCapturedDiffAndRefusesMissingEvidence(t *testing.T) {
	for _, evidence := range []string{"captured", "absent", "wrong-stage"} {
		t.Run(evidence, func(t *testing.T) {
			endpoint, _ := fakeBlobPlane(t)
			t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
			t.Setenv(dispatcher.EnvPodToken, "pod-token")
			t.Setenv(dispatcher.EnvRunID, "child-review")
			t.Setenv(dispatcher.EnvStage, "review")
			t.Setenv(dispatcher.EnvAttempt, "1")
			t.Setenv(dispatcher.EnvStageCapabilities, "")
			t.Setenv(dispatcher.EnvCheckoutCapability, "")
			t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceScratch))
			t.Setenv(dispatcher.EnvDaemonAPI, "")
			t.Chdir(t.TempDir())
			const diff = "diff --git a/file b/file\n+captured host change\n"
			kit := &agentickit.Kit{
				Envelope: apiv1.InvocationEnvelope{RunID: "child-review", TaskID: "child-review:review", Goober: "coder", Goal: "gate: review"},
				Mode:     agentickit.ModeReview, ReviewRequiresDiff: true,
				Goobers:      map[string]apiv1.GooberSpec{"coder": {Harness: apiv1.HarnessCopilot}},
				Instructions: map[string]string{"coder": "review the change"},
			}
			if evidence != "absent" {
				name := "review.diff"
				if evidence == "wrong-stage" {
					name = "other.diff"
				}
				pointer := seedBlobPlane(t, endpoint, name, []byte(diff))
				pointer.Artifact.MediaType = "text/x-diff"
				kit.Envelope.ContextPointers = []apiv1.ContextPointer{pointer}
			}
			data, digest, err := agentickit.Marshal(kit)
			if err != nil {
				t.Fatal(err)
			}
			if err = (&dispatcher.BlobClient{BaseURL: endpoint, Token: "pod-token"}).Put(t.Context(), digest, data); err != nil {
				t.Fatal(err)
			}
			t.Setenv(dispatcher.EnvAgenticKitDigest, digest)
			sessions := 0
			installFakeHarness(t, func(_ context.Context, req harness.RunRequest) error {
				sessions++
				paths, err := filepath.Glob(filepath.Join(req.Workspace, ".goobers", "context", "*review.diff"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("captured diff missing from reviewer workspace: %v %v", paths, err)
				}
				content, err := os.ReadFile(paths[0])
				if err != nil || string(content) != diff {
					t.Fatalf("reviewer saw different evidence: %q %v", content, err)
				}
				return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "reviewed captured evidence"})
			})
			ctx := context.WithValue(t.Context(), isolatedChildKey{}, true)
			var stderr strings.Builder
			out := runAgenticStage(ctx, io.Discard, &stderr)
			if evidence == "captured" {
				if sessions != 1 || out.Verdict == nil || out.Verdict.Decision != apiv1.VerdictPass {
					t.Fatalf("captured review failed: %+v, sessions=%d, %s", out, sessions, stderr.String())
				}
			} else if sessions != 0 || out.Result.Error == nil || out.Result.Error.Code != "reviewer_diff_missing" || out.Result.Error.Retryable {
				t.Fatalf("missing evidence reached reviewer: %+v, sessions=%d", out, sessions)
			}
		})
	}
}
