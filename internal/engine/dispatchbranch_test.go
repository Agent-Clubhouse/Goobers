package engine

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// dispatchbranch_test.go covers #392 on the mode-3 seam: the run's REBOUND
// workspace branch (and the stage's syncBase declaration) reaching the pod,
// and the continuity record keying on that branch.
//
// The bug these pin: pr-remediation rebinds the run's workspace branch to the
// claimed PR's head, so every stage after gather-pr-context operates on the
// PR's branch. The local runner has threaded that binding since #392; the
// dispatch seam never carried it, so a pod stage derived the RUN branch from
// workflow + run id, checked out a branch with none of the PR's commits, and
// remediated something nobody was reviewing — while reporting success.

const reboundBranch = "goobers/impl/pr-head"

// rebindingDeterministic emits the well-known workspaceBranch output from one
// named stage, which is exactly how gather-pr-context rebinds a run.
type rebindingDeterministic struct {
	stage  string
	branch string
}

func (d *rebindingDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}
	if strings.TrimPrefix(env.TaskID, env.RunID+":") == d.stage {
		result.Outputs = map[string]interface{}{runner.WorkspaceBranchOutput: d.branch}
	}
	return result, nil
}

// The headline: a pod stage dispatched AFTER a rebinding stage carries the
// rebound branch, and one dispatched BEFORE carries none — the pod's own
// derivation is right until the moment it stops being.
func TestPodDispatchCarriesTheReboundWorkspaceBranch(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "before",
		Tasks: []apiv1.Task{
			podTask("before", "select", nil),
			{Name: "select", Type: apiv1.TaskDeterministic, Goal: "claim a PR",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "after"},
			podTask("after", "", nil),
		},
	}
	in := runInput("rebound-branch", spec)
	in.Placements = []PinnedPlacement{remotePin("before"), remotePin("after")}
	surrenders := surrenderStore(t)
	putSurrendered(t, surrenders, in.RunID, "before", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	putSurrendered(t, surrenders, in.RunID, "after", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}
	workspaces := testWorkspaces(t)
	det := &rebindingDeterministic{stage: "select", branch: reboundBranch}

	executeForProjection(t, in, &Activities{Det: det, Workspaces: workspaces, Dispatcher: fake, Surrenders: surrenders}, false)

	attempts, _ := fake.recorded()
	if len(attempts) != 2 {
		t.Fatalf("pod attempts = %d, want two", len(attempts))
	}
	if attempts[0].Stage != "before" || attempts[0].WorkspaceBranch != "" {
		t.Errorf("pre-rebind attempt = {stage:%q branch:%q}, want the run's own derived branch (no stamp)", attempts[0].Stage, attempts[0].WorkspaceBranch)
	}
	if attempts[1].Stage != "after" || attempts[1].WorkspaceBranch != reboundBranch {
		t.Fatalf("post-rebind attempt = {stage:%q branch:%q}, want %q — without it the pod checks out the run branch and remediates a tree nobody is reviewing",
			attempts[1].Stage, attempts[1].WorkspaceBranch, reboundBranch)
	}
	// The self arm's request is the parity reference: both substrates are told
	// the same thing about the same run.
	if got := requestFor(t, workspaces.provisioned(), "select").WorkspaceBranch; got != "" {
		t.Errorf("the rebinding stage's own workspace branch = %q, want empty (it rebinds for LATER stages)", got)
	}
}

// A read-only stage reads the pinned base on every substrate, so the rebound
// branch must not be stamped for it — the local runner's read-only arm
// provisions a detached checkout and ignores the binding.
func TestReadOnlyPodStageIsNotStampedWithTheReboundBranch(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "select",
		Tasks: []apiv1.Task{
			{Name: "select", Type: apiv1.TaskDeterministic, Goal: "claim a PR",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "read"},
			{Name: "read", Type: apiv1.TaskDeterministic, Goal: "read",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepoReadOnly}},
		},
	}
	in := runInput("rebound-readonly", spec)
	in.Placements = []PinnedPlacement{remotePin("read")}
	surrenders := surrenderStore(t)
	putSurrendered(t, surrenders, in.RunID, "read", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	executeForProjection(t, in, &Activities{
		Det:        &rebindingDeterministic{stage: "select", branch: reboundBranch},
		Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders,
	}, false)

	attempts, _ := fake.recorded()
	if len(attempts) != 1 || attempts[0].WorkspaceBranch != "" {
		t.Fatalf("read-only attempt = %+v, want no stamped workspace branch", attempts)
	}
}

// syncBase (#813) is a shipped DSL feature with no pod-side path before this:
// the declaration is pinned in the definition, so only the dispatch payload
// can carry it to a pod.
func TestPodDispatchCarriesSyncBase(t *testing.T) {
	for _, tc := range []struct {
		name     string
		syncBase bool
	}{{"declared", true}, {"absent", false}} {
		t.Run(tc.name, func(t *testing.T) {
			spec := apiv1.WorkflowSpec{
				Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "rebase",
				Tasks: []apiv1.Task{podTask("rebase", "", func(task *apiv1.Task) { task.Run.SyncBase = tc.syncBase })},
			}
			in := runInput("syncbase-"+tc.name, spec)
			in.Placements = []PinnedPlacement{remotePin("rebase")}
			surrenders := surrenderStore(t)
			putSurrendered(t, surrenders, in.RunID, "rebase", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
			fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

			executeForProjection(t, in, &Activities{Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders}, false)

			attempts, _ := fake.recorded()
			if len(attempts) != 1 {
				t.Fatalf("pod attempts = %d, want one", len(attempts))
			}
			if attempts[0].SyncBase != tc.syncBase {
				t.Fatalf("attempt SyncBase = %v, want %v — a syncBase stage that silently skips the merge builds against a stale base and calls it success", attempts[0].SyncBase, tc.syncBase)
			}
		})
	}
}

// The selector's branch key (#392 half of #3845): a bundle is a THIN git
// bundle of base..tip, so it is meaningful only on the branch whose history
// holds its prerequisite. A pre-rebind entry must not be handed to a
// post-rebind consumer.
func TestSelectDeltaKeysOnTheWorkspaceBranch(t *testing.T) {
	record := []continuityEntry{
		{Stage: "seed", Attempt: 1, Digest: deltaA},
		{Stage: "rework", Attempt: 1, Digest: deltaB, Branch: reboundBranch},
	}
	t.Run("a consumer on the rebound branch sees only that branch's entries", func(t *testing.T) {
		got, err := selectDelta(record, "verify", nil, reboundBranch)
		if err != nil || got.Digest != deltaB {
			t.Fatalf("selectDelta = %+v, %v; want rework's entry %s", got, err, deltaB)
		}
	})
	t.Run("a consumer on the run branch does not see a rebound entry", func(t *testing.T) {
		got, err := selectDelta(record, "verify", nil, "")
		if err != nil || got.Digest != deltaA {
			t.Fatalf("selectDelta = %+v, %v; want seed's entry %s", got, err, deltaA)
		}
	})
	t.Run("no entry on the consumer's branch selects nothing", func(t *testing.T) {
		got, err := selectDelta(record[:1], "verify", nil, reboundBranch)
		if err != nil || got.Digest != "" {
			t.Fatalf("selectDelta = %+v, %v; want nothing — the pre-rebind bundle cannot land on the PR head", got, err)
		}
	})
	// The WF022 runtime refusal is aimed at an undeclared producer on THIS
	// branch. A producer the rebind already made irrelevant must not fire it,
	// or every 3.0 lane that rebinds fails the moment it declares repoFrom.
	t.Run("an off-branch producer does not trip the repoFrom refusal", func(t *testing.T) {
		got, err := selectDelta(record, "verify", []string{"seed"}, "")
		if err != nil || got.Digest != deltaA {
			t.Fatalf("selectDelta = %+v, %v; want seed's entry with no refusal", got, err)
		}
	})
}

// End to end through the walk: a pod commits on the run branch, a stage
// rebinds, and the next pod is handed NO delta rather than a bundle whose
// prerequisite is on the abandoned branch.
func TestReboundRunDoesNotCarryPreRebindDeltaToTheNextPod(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "seed",
		Tasks: []apiv1.Task{
			podTask("seed", "select", nil),
			{Name: "select", Type: apiv1.TaskDeterministic, Goal: "claim a PR",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "rework"},
			podTask("rework", "", nil),
		},
	}
	in := projectionInput("rebound-drops-delta", spec)
	in.Placements = []PinnedPlacement{remotePin("seed"), remotePin("rework")}
	surrenders := surrenderStore(t)
	surrenderDelta(t, surrenders, in.RunID, "seed", 1, deltaA)
	putSurrendered(t, surrenders, in.RunID, "rework", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	proj := executeForProjection(t, in, &Activities{
		Det:        &rebindingDeterministic{stage: "select", branch: reboundBranch},
		Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders,
	}, false)

	attempts, _ := fake.recorded()
	if len(attempts) != 2 {
		t.Fatalf("pod attempts = %d, want two", len(attempts))
	}
	if attempts[1].WorkspaceDelta != "" {
		t.Fatalf("the post-rebind pod was handed delta %q from a stage on the run branch; git refuses its prerequisite on the PR head, and where it would not, applying it resets the PR onto another line of work", attempts[1].WorkspaceDelta)
	}
	// The publication is journaled with the branch it was made on, which is
	// what a far-side reader compares against the pod's checkout.
	var sawSeedPublication bool
	for _, ev := range deltaEvents(proj) {
		if ev.Stage == "seed" && ev.Runner["digest"] == deltaA {
			sawSeedPublication = true
			if branch, ok := ev.Runner["branch"]; ok {
				t.Errorf("seed's publication names branch %v, want none — it ran before the rebind", branch)
			}
		}
	}
	if !sawSeedPublication {
		t.Fatalf("no published event for seed: %+v", deltaEvents(proj))
	}
}

// The other direction, and the one that makes the branch key a KEY rather than
// a filter that only ever drops things: continuity must still WORK after a
// rebind. Two pods both running on the PR head hand work to each other exactly
// as two pods on a run branch do.
//
// This is what pins the branch onto the recorded entry at publication time. Key
// the entry with the empty string instead — record the consumer's branch, or
// nothing at all — and the post-rebind consumer's filter drops its own
// predecessor's bundle: pr-remediation's implement stage would silently run
// without the commits remediation-checkpoint just made, which is the same
// silent-wrong-result the record exists to prevent, arrived at from the
// opposite side.
func TestPostRebindPodsCarryDeltaToEachOther(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "select",
		Tasks: []apiv1.Task{
			{Name: "select", Type: apiv1.TaskDeterministic, Goal: "claim a PR",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "rework"},
			podTask("rework", "verify", nil),
			podTask("verify", "", nil),
		},
	}
	in := projectionInput("rebound-carries-delta", spec)
	in.Placements = []PinnedPlacement{remotePin("rework"), remotePin("verify")}
	surrenders := surrenderStore(t)
	surrenderDelta(t, surrenders, in.RunID, "rework", 1, deltaA)
	putSurrendered(t, surrenders, in.RunID, "verify", 1, dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}})
	fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

	proj := executeForProjection(t, in, &Activities{
		Det:        &rebindingDeterministic{stage: "select", branch: reboundBranch},
		Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders,
	}, false)

	attempts, _ := fake.recorded()
	if len(attempts) != 2 {
		t.Fatalf("pod attempts = %d, want two", len(attempts))
	}
	for _, a := range attempts {
		if a.WorkspaceBranch != reboundBranch {
			t.Fatalf("stage %q dispatched on branch %q, want the rebound %q", a.Stage, a.WorkspaceBranch, reboundBranch)
		}
	}
	if attempts[1].WorkspaceDelta != deltaA {
		t.Fatalf("verify was handed delta %q, want %q — its own predecessor on the SAME branch published it, so dropping it runs the stage without commits it must build on", attempts[1].WorkspaceDelta, deltaA)
	}
	// events.jsonl is the far-side reader's record: publication and selection
	// must agree on the branch, which is precisely what makes a mismatch
	// visible rather than inferred.
	var published, selected bool
	for _, ev := range deltaEvents(proj) {
		if ev.Runner["digest"] != deltaA {
			continue
		}
		switch ev.Runner["action"] {
		case string(journal.WorkspaceDeltaPublished):
			published = true
			if ev.Runner["branch"] != reboundBranch {
				t.Errorf("published event names branch %v, want %q", ev.Runner["branch"], reboundBranch)
			}
		case string(journal.WorkspaceDeltaSelected):
			selected = true
			if ev.Runner["branch"] != reboundBranch {
				t.Errorf("selected event names branch %v, want %q", ev.Runner["branch"], reboundBranch)
			}
		}
	}
	if !published || !selected {
		t.Fatalf("published=%v selected=%v; want both journaled on the rebound branch: %+v", published, selected, deltaEvents(proj))
	}
}

// The gate-shaped twin of TestPodDispatchCarriesTheReboundWorkspaceBranch
// (decision 001 rulings 7–8 meets #392): a placed reviewer gate goes through
// ActDispatchStage exactly as a placed task does, and DispatchStage's own
// writable-repo gate is what has to keep the two arms from disagreeing —
// there is no separate "is this a gate" branch in the stamping code, so this
// pins that dispatchRemoteGate actually reaches it. The default agentic-gate
// workspace is the writable repo (ReviewGoober's own reading of an unset
// AgenticGate.Workspace); repo-readonly is the arm that must carry none, the
// same as a read-only TASK.
func TestPlacedGateCarriesTheReboundWorkspaceBranch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		workspace  apiv1.WorkspaceMode
		wantBranch string
	}{
		{name: "default writable repo carries the rebound branch", workspace: "", wantBranch: reboundBranch},
		{name: "declared repo-readonly carries none", workspace: apiv1.WorkspaceRepoReadOnly, wantBranch: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := placedGateSpec()
			spec.Gates[0].Agentic.Workspace = tc.workspace
			// selectedWorkspaceBranch only ever reads a DETERMINISTIC task's
			// outputs (engine.go): "implement" becomes the rebinding stage, the
			// same shape as "select" in TestPodDispatchCarriesTheReboundWorkspaceBranch,
			// so the gate immediately after it is the run's first post-rebind
			// dispatch.
			spec.Tasks[0] = apiv1.Task{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "claim a PR",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "review",
			}
			in := projectionInput("placed-gate-branch-"+string(tc.workspace), spec)
			in.DSLVersion = "3.0"
			in.GateGooberCapabilities = map[string][]string{"reviewer": {"agent:model"}}
			in.Placements = []PinnedPlacement{remoteGatePin()}
			surrenders := surrenderStore(t)
			putSurrendered(t, surrenders, in.RunID, "review", 1, reviewSurrender(apiv1.Verdict{Decision: apiv1.VerdictPass}))
			fake := &fakeStageDispatcher{report: dispatcher.Report{Runner: "linux-agentic", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}}

			executeForProjection(t, in, &Activities{
				Goober: refusingReviewer(t), Det: &rebindingDeterministic{stage: "implement", branch: reboundBranch},
				Workspaces: testWorkspaces(t), Dispatcher: fake, Surrenders: surrenders,
			}, false)

			attempts, _ := fake.recorded()
			if len(attempts) != 1 || attempts[0].Stage != "review" {
				t.Fatalf("attempts = %+v, want exactly the gate's review attempt", attempts)
			}
			if got := attempts[0].WorkspaceBranch; got != tc.wantBranch {
				t.Fatalf("review attempt WorkspaceBranch = %q, want %q — a placed gate must read the rebound branch on a writable workspace and none on repo-readonly, the same rule DispatchStage already applies to a task", got, tc.wantBranch)
			}
		})
	}
}
