package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/harness"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

// dispatchreview_test.go is the pod half of decision 001 rulings 7–8: a kit
// stamped mode: review drives the goober through exec.Review, the verdict is
// surrendered beside a bare success status, the reviewer diff is computed by
// THIS binary from the checkout the delta was applied to (#301 parity), and
// the checkout credential never reaches the reviewer's environment.

// The mode rides the ATTEMPT into the kit: Review stamps review, everything
// else — including a plain agentic task — stamps invoke, and an attempt with
// no mode at all decodes as invoke on the pod.
func TestKitModeFollowsTheAttempt(t *testing.T) {
	if got := kitModeFor(dispatcher.Attempt{Agentic: true, Review: true}); got != agentickit.ModeReview {
		t.Fatalf("kitModeFor(review attempt) = %q, want %q", got, agentickit.ModeReview)
	}
	if got := kitModeFor(dispatcher.Attempt{Agentic: true}); got != agentickit.ModeInvoke {
		t.Fatalf("kitModeFor(task attempt) = %q, want %q", got, agentickit.ModeInvoke)
	}
	var legacy agentickit.Kit
	if err := json.Unmarshal([]byte(`{"envelope":{}}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.IsReview() {
		t.Fatal("a kit published before Mode existed must read as an invocation")
	}
}

// podPlanes is one fake daemon for a pod test: it serves the credential
// plane (a scripted checkout credential), accepts journal-plane emits, and
// captures the surrender PUT so the surrendered document can be read back.
type podPlanes struct {
	url            string
	checkoutToken  string
	mu             sync.Mutex
	surrendered    []byte
	surrenderPath  string
	credentialReqs []string
}

func newPodPlanes(t *testing.T, checkoutToken string) *podPlanes {
	t.Helper()
	p := &podPlanes{checkoutToken: checkoutToken}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		defer p.mu.Unlock()
		switch {
		case r.URL.Path == apicontract.CredentialResolvePath:
			p.credentialReqs = append(p.credentialReqs, string(body))
			var req struct {
				Capabilities []string `json:"capabilities"`
			}
			_ = json.Unmarshal(body, &req)
			creds := make([]dispatcher.MintedCredential, 0, len(req.Capabilities))
			for _, c := range req.Capabilities {
				creds = append(creds, dispatcher.MintedCredential{Capability: c, Value: p.checkoutToken})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"credentials": creds})
		case strings.HasSuffix(r.URL.Path, "/surrender"):
			p.surrendered = body
			p.surrenderPath = r.URL.Path
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(server.Close)
	p.url = server.URL
	return p
}

// publishReviewKit publishes a review-mode kit for the "coder" goober to the
// fake blob plane and stamps its digest, exactly as the dispatcher does.
func publishReviewKit(t *testing.T, endpoint string, env apiv1.InvocationEnvelope, mode agentickit.Mode) {
	t.Helper()
	kit := &agentickit.Kit{
		Envelope:     env,
		Mode:         mode,
		Goobers:      map[string]apiv1.GooberSpec{"coder": {Harness: apiv1.HarnessCopilot}},
		Instructions: map[string]string{"coder": "review the change"},
	}
	data, digest, err := agentickit.Marshal(kit)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&dispatcher.BlobClient{BaseURL: endpoint, Token: "pod-token"}).Put(context.Background(), digest, data); err != nil {
		t.Fatalf("publish kit: %v", err)
	}
	t.Setenv(dispatcher.EnvAgenticKitDigest, digest)
}

// installFakeHarness substitutes the pod's harness registry with a copilot
// adapter that runs act, through the same seam the context tests use.
func installFakeHarness(t *testing.T, act func(context.Context, harness.RunRequest) error) {
	t.Helper()
	registry := harness.NewRegistry()
	if err := registry.RegisterAs(string(apiv1.HarnessCopilot), &harnesstest.FakeAdapter{Act: act}); err != nil {
		t.Fatal(err)
	}
	previous := podHarnessRegistry
	podHarnessRegistry = func(map[string]string, []string, map[string][]string, string, string, bool, func(context.Context) (string, error)) (*harness.Registry, error) {
		return registry, nil
	}
	t.Cleanup(func() { podHarnessRegistry = previous })
}

// A review-mode kit drives exec.Review — the harness sees ModeReview and a
// verdict completion path — and the pod surrenders the verdict beside a bare
// success; an invoke-mode kit against the same fixture drives Invoke and
// surrenders no verdict.
func TestRunAgenticStageReviewModeSurrendersVerdict(t *testing.T) {
	endpoint, _ := fakeBlobPlane(t)
	t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvRunID, "run-review")
	t.Setenv(dispatcher.EnvStage, "review")
	t.Setenv(dispatcher.EnvAttempt, "1")
	t.Setenv(dispatcher.EnvStageCapabilities, "")
	t.Setenv(dispatcher.EnvCheckoutCapability, "")
	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceScratch))
	t.Setenv(dispatcher.EnvDaemonAPI, "")
	t.Chdir(t.TempDir())
	env := apiv1.InvocationEnvelope{RunID: "run-review", TaskID: "run-review:review", Goober: "coder", Goal: "gate: review"}

	var modes []harness.Mode
	installFakeHarness(t, func(_ context.Context, req harness.RunRequest) error {
		modes = append(modes, req.Mode)
		if req.Mode == harness.ModeReview {
			return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "one more pass"})
		}
		return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "implemented"})
	})

	publishReviewKit(t, endpoint, env, agentickit.ModeReview)
	got := runAgenticStage(context.Background(), &strings.Builder{}, &strings.Builder{})
	if got.Result.Status != apiv1.ResultSuccess {
		t.Fatalf("review outcome = %+v, want a bare success beside the verdict", got.Result)
	}
	if got.Verdict == nil || got.Verdict.Decision != apiv1.VerdictNeedsChanges {
		t.Fatalf("review verdict = %+v, want the harness's needs-changes verdict surrendered", got.Verdict)
	}
	if len(modes) != 1 || modes[0] != harness.ModeReview {
		t.Fatalf("harness modes = %v, want exactly one ModeReview session (exec.Review, never Invoke)", modes)
	}

	// Control: the same fixture with the mode left at its pre-Mode zero value
	// is an invocation.
	publishReviewKit(t, endpoint, env, "")
	got = runAgenticStage(context.Background(), &strings.Builder{}, &strings.Builder{})
	if got.Verdict != nil || got.Result.Summary != "implemented" {
		t.Fatalf("invoke outcome = %+v (verdict %+v), want the invocation's result and no verdict", got.Result, got.Verdict)
	}
	if len(modes) != 2 || modes[1] != harness.ModeInvoke {
		t.Fatalf("harness modes = %v, want the second session in ModeInvoke", modes)
	}
}

// THE FAR-SIDE SHAPE, at the pod's own seam: a review pod checks the run's
// repository out, applies the subject stage's delta, computes `git diff
// <base>...HEAD` ITSELF, journals it as <gate>/reviewer-diff.patch and hands
// the reviewer a "<gate>.diff" pointer whose bytes the harness materializes
// into the workspace — so the reviewer judges the commit a pod-side implement
// stage made, not base. The checkout credential the pod minted for the clone
// is nowhere in the reviewer's environment, and the pod surrenders the
// verdict with NO workspace delta even though the reviewer's checkout is
// writable and carries commits.
func TestReviewPodComputesTheReviewerDiffFromTheAppliedDelta(t *testing.T) {
	origin := initBareOrigin(t)
	endpoint, _ := fakeBlobPlane(t)
	prev := checkoutCloneURL
	checkoutCloneURL = func(apiv1.RepoRef) (string, error) { return origin, nil }
	t.Cleanup(func() { checkoutCloneURL = prev })

	// The subject: a pod-side implement stage that committed on the run
	// branch and published its delta — the commit the reviewer must see.
	t.Setenv(dispatcher.EnvBlobEndpoint, endpoint)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
	t.Setenv(executor.BaseBranchEnvVar, "main")
	const branch = "e2e/gate-probe/run-gate"
	implement := filepath.Join(t.TempDir(), "implement")
	runGitT(t, filepath.Dir(implement), "clone", "--branch", "main", origin, implement)
	digest, implementHead := publishDeltaFrom(t, implement, branch, "carried.txt", "the pod-side change\n")

	const checkoutToken = "chk-t0ken-0123456789abcdef"
	planes := newPodPlanes(t, checkoutToken)
	t.Setenv(dispatcher.EnvDaemonAPI, planes.url)
	t.Setenv(dispatcher.EnvRunID, "run-gate")
	t.Setenv(dispatcher.EnvStage, "review")
	t.Setenv(dispatcher.EnvAttempt, "1")
	t.Setenv(dispatcher.EnvWorkflow, "gate-probe")
	t.Setenv(dispatcher.EnvGaggle, "e2e")
	t.Setenv(executor.RepoProviderEnvVar, string(apiv1.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "acme")
	t.Setenv(executor.RepoNameEnvVar, "widget")
	t.Setenv(executor.BranchNamespaceEnvVar, "e2e/")
	t.Setenv(dispatcher.EnvWorkspaceDelta, digest)
	t.Setenv(dispatcher.EnvStageCapabilities, "")
	t.Setenv(dispatcher.EnvCheckoutCapability, "repo:push")
	workspace := t.TempDir()
	t.Chdir(workspace)

	var (
		seenPointer *apiv1.ContextPointer
		seenDiff    string
		leaked      []string
	)
	installFakeHarness(t, func(_ context.Context, req harness.RunRequest) error {
		if req.Mode != harness.ModeReview {
			return errors.New("review kit drove the harness in " + string(req.Mode))
		}
		for i := range req.Envelope.ContextPointers {
			if req.Envelope.ContextPointers[i].Name == "review.diff" {
				seenPointer = &req.Envelope.ContextPointers[i]
			}
		}
		matches, _ := filepath.Glob(filepath.Join(req.Workspace, ".goobers", "context", "*review.diff"))
		if len(matches) == 1 {
			data, err := os.ReadFile(matches[0])
			if err != nil {
				return err
			}
			seenDiff = string(data)
		}
		for _, entry := range os.Environ() {
			if strings.Contains(entry, checkoutToken) {
				leaked = append(leaked, entry)
			}
		}
		// A reviewer that commits is misbehaving; the pod must still never
		// carry that commit forward.
		if err := os.WriteFile(filepath.Join(req.Workspace, "reviewer-scribble.txt"), []byte("not evidence\n"), 0o644); err != nil {
			return err
		}
		runGitT(t, req.Workspace, "config", "user.name", "reviewer")
		runGitT(t, req.Workspace, "config", "user.email", "reviewer@example.com")
		runGitT(t, req.Workspace, "add", "reviewer-scribble.txt")
		runGitT(t, req.Workspace, "commit", "-q", "-m", "a reviewer must not commit")
		return harnesstest.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "saw the carried change"})
	})
	publishReviewKit(t, endpoint, apiv1.InvocationEnvelope{RunID: "run-gate", TaskID: "run-gate:review", Goober: "coder", Goal: "gate: review"}, agentickit.ModeReview)

	var stderr strings.Builder
	if code := runDispatchExecContext(context.Background(), io.Discard, &stderr); code != 0 {
		t.Fatalf("dispatch-exec exit = %d\nstderr:\n%s", code, stderr.String())
	}

	// The checkout landed on the subject's commit before the diff was taken.
	if head := strings.TrimSpace(runGitOutputT(t, workspace, "rev-parse", branch)); !strings.Contains(runGitOutputT(t, workspace, "log", "--format=%H", branch), implementHead) || head == "" {
		t.Fatalf("review checkout does not carry the implement commit %s:\n%s", implementHead, stderr.String())
	}
	if seenPointer == nil || seenPointer.Artifact == nil || seenPointer.Artifact.MediaType != "text/x-diff" {
		t.Fatalf("reviewer envelope carried no review.diff pointer (%+v):\n%s", seenPointer, stderr.String())
	}
	if !strings.Contains(seenDiff, "+the pod-side change") || !strings.Contains(seenDiff, "carried.txt") {
		t.Fatalf("the reviewer's materialized diff evidence does not show the pod-side commit:\n%q\nstderr:\n%s", seenDiff, stderr.String())
	}
	if len(leaked) != 0 {
		t.Fatalf("the checkout credential reached the reviewer's environment: %v", leaked)
	}
	if !strings.Contains(stderr.String(), "reviewer diff: recorded review/reviewer-diff.patch") {
		t.Errorf("pod stderr does not announce the journaled reviewer diff (the far-side evidence line):\n%s", stderr.String())
	}

	planes.mu.Lock()
	defer planes.mu.Unlock()
	var surrendered dispatcher.SurrenderedResult
	if err := json.Unmarshal(planes.surrendered, &surrendered); err != nil {
		t.Fatalf("decode surrendered document %q: %v", planes.surrendered, err)
	}
	if surrendered.Verdict == nil || surrendered.Verdict.Decision != apiv1.VerdictPass {
		t.Fatalf("surrendered verdict = %+v, want the reviewer's pass", surrendered.Verdict)
	}
	if surrendered.Result.Status != apiv1.ResultSuccess {
		t.Fatalf("surrendered status = %q, want success beside the verdict", surrendered.Result.Status)
	}
	if surrendered.WorkspaceDelta != "" || surrendered.WorkspaceDeltaUnchanged {
		t.Fatalf("a review surrendered a workspace delta (%q, unchanged=%t); a reviewer never publishes, and its scribble commit must die with the pod", surrendered.WorkspaceDelta, surrendered.WorkspaceDeltaUnchanged)
	}
	if len(planes.credentialReqs) != 1 || !strings.Contains(planes.credentialReqs[0], `"repo:push"`) || !strings.Contains(planes.credentialReqs[0], `"review"`) {
		t.Fatalf("credential plane requests = %v, want exactly the checkout mint for stage review", planes.credentialReqs)
	}
}

// The two nil cases mirror the local runner's recordReviewerDiff: no repo
// workspace, or a checkout that carries no change against base, attaches
// nothing and is not an error.
func TestRecordPodReviewerDiffAttachesNothingWithoutAChange(t *testing.T) {
	t.Setenv(dispatcher.EnvDaemonAPI, "")
	t.Setenv(executor.BaseBranchEnvVar, "main")

	t.Run("scratch workspace", func(t *testing.T) {
		t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceScratch))
		ptr, err := recordPodReviewerDiff(context.Background(), t.TempDir(), t.TempDir(), "review", nil, &strings.Builder{})
		if err != nil || ptr != nil {
			t.Fatalf("scratch: pointer %+v err %v, want nothing to attach", ptr, err)
		}
	})
	t.Run("checkout at base", func(t *testing.T) {
		t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
		origin := initBareOrigin(t)
		ws := filepath.Join(t.TempDir(), "ws")
		runGitT(t, filepath.Dir(ws), "clone", "--branch", "main", origin, ws)
		var stderr strings.Builder
		ptr, err := recordPodReviewerDiff(context.Background(), ws, t.TempDir(), "review", nil, &stderr)
		if err != nil || ptr != nil {
			t.Fatalf("base checkout: pointer %+v err %v, want nothing to attach", ptr, err)
		}
		if !strings.Contains(stderr.String(), "is empty; no diff evidence attached") {
			t.Errorf("stderr does not say why no evidence was attached:\n%s", stderr.String())
		}
	})
	t.Run("a change is attached and resolvable where the harness looks", func(t *testing.T) {
		t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepo))
		origin := initBareOrigin(t)
		ws := filepath.Join(t.TempDir(), "ws")
		runGitT(t, filepath.Dir(ws), "clone", "--branch", "main", origin, ws)
		runGitT(t, ws, "checkout", "-b", "e2e/wf/run-x")
		runGitT(t, ws, "config", "user.name", "t")
		runGitT(t, ws, "config", "user.email", "t@example.com")
		if err := os.WriteFile(filepath.Join(ws, "changed.txt"), []byte("secret-value-0123456789abcdef\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitT(t, ws, "add", "changed.txt")
		runGitT(t, ws, "commit", "-q", "-m", "change")
		runsDir := t.TempDir()
		creds := []dispatcher.MintedCredential{{Capability: "repo:push", Value: "secret-value-0123456789abcdef"}}
		ptr, err := recordPodReviewerDiff(context.Background(), ws, runsDir, "review", creds, &strings.Builder{})
		if err != nil || ptr == nil || ptr.Artifact == nil {
			t.Fatalf("pointer %+v err %v, want the diff evidence", ptr, err)
		}
		if ptr.Name != "review.diff" {
			t.Fatalf("pointer name = %q, want review.diff", ptr.Name)
		}
		// Resolvable through the exact primitive the harness uses, digest
		// re-verified against the staged bytes.
		data, err := ptr.Artifact.Resolve(runsDir)
		if err != nil {
			t.Fatalf("the harness could not resolve the staged evidence: %v", err)
		}
		if strings.Contains(string(data), "secret-value-0123456789abcdef") {
			t.Fatal("a minted credential captured in a commit reached the journaled diff unscrubbed")
		}
		if !strings.Contains(string(data), "changed.txt") {
			t.Fatalf("diff does not name the changed file:\n%s", data)
		}
	})
}
