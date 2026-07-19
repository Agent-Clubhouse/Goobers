package workflow

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestShippedMergeReviewWorkflowsWirePostMergeChain(t *testing.T) {
	tests := []struct {
		name string
		root string
	}{
		{
			name: "selfhost",
			root: filepath.Join("..", "..", "selfhost", "gaggles", "goobers"),
		},
		{
			name: "acme-web",
			root: filepath.Join("..", "..", "config-examples", "gaggles", "acme-web"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(tt.root, "workflows", "merge-review.yaml"))
			if err != nil {
				t.Fatalf("read workflow: %v", err)
			}
			var w apiv1.Workflow
			if err := yaml.Unmarshal(raw, &w); err != nil {
				t.Fatalf("unmarshal workflow: %v", err)
			}

			raw, err = os.ReadFile(filepath.Join(tt.root, "goobers", "reviewer", "goober.yaml"))
			if err != nil {
				t.Fatalf("read reviewer: %v", err)
			}
			var reviewer apiv1.Goober
			if err := yaml.Unmarshal(raw, &reviewer); err != nil {
				t.Fatalf("unmarshal reviewer: %v", err)
			}
			registered := false
			for _, workflowName := range reviewer.Spec.Workflows {
				if workflowName == "merge-review" {
					registered = true
					break
				}
			}
			if !registered {
				t.Error("reviewer is not registered for merge-review")
			}

			m, err := Compile(
				Definition{Name: w.Name, Version: 1, Spec: w.Spec},
				WithGoobers(map[string]apiv1.GooberSpec{"reviewer": reviewer.Spec}),
				WithKnownChecks([]string{"output-equals", "land-outcome", "queue-outcome"}),
			)
			if err != nil {
				t.Fatalf("compile workflow: %v", err)
			}

			review, ok := m.Gate("review")
			if !ok {
				t.Fatal("review gate not found")
			}
			// #833: needs-changes routes through elect-lander (winner-election)
			// before parking. #825: pass now also routes through apply-verdict
			// (every verdict is published as a native GitHub review before any
			// merge decision) instead of straight to merge-pr.
			wantReviewBranches := map[string]string{
				"pass":          "apply-verdict",
				"needs-changes": "elect-lander",
				"fail":          "apply-verdict",
			}
			if !reflect.DeepEqual(review.Branches, wantReviewBranches) {
				t.Errorf("review branches = %v, want %v", review.Branches, wantReviewBranches)
			}

			// #833: elect-lander runs the deterministic winner-election and hands
			// off to elect-gate, which routes the crowned lander straight to
			// merge-pr (deliberately bypassing #825's apply-verdict/
			// published-verdict native-review chain on this override path —
			// the verdict that got it here was needs-changes, not pass) and
			// everything else to apply-verdict (park blocked-on-sibling /
			// needs-remediation).
			electLander, ok := m.Task("elect-lander")
			if !ok {
				t.Fatal("elect-lander task not found")
			}
			if electLander.Run == nil || !reflect.DeepEqual(electLander.Run.Command, []string{"goobers", "elect-lander"}) {
				t.Errorf("elect-lander command = %+v, want [goobers elect-lander]", electLander.Run)
			}
			if electLander.Next != "elect-gate" {
				t.Errorf("elect-lander.next = %q, want elect-gate", electLander.Next)
			}
			electGate, ok := m.Gate("elect-gate")
			if !ok {
				t.Fatal("elect-gate gate not found")
			}
			// BOTH branches route to apply-verdict. Election means "those
			// siblings no longer block you", not "merge regardless of review",
			// so the crowned lander resolves into an ordinary verdict rather
			// than acquiring a separate merge authority: apply-verdict derives
			// the pass (electedLanderPass) and published-verdict routes it on
			// to merge-pr, same run, same path every other merged PR takes.
			//
			// The former pass -> merge-pr bypass could not work — merge-pr
			// builds its commit message from a pass verdict comment pinned to
			// the current SHAs, and the bypass posted no verdict comment at
			// all, so merge-pr exited 1 every cycle a cluster existed.
			wantElectBranches := map[string]string{
				"pass": "apply-verdict",
				"fail": "apply-verdict",
			}
			if !reflect.DeepEqual(electGate.Branches, wantElectBranches) {
				t.Errorf("elect-gate branches = %v, want %v", electGate.Branches, wantElectBranches)
			}

			// #825: apply-verdict now publishes every verdict as a native
			// GitHub review before published-verdict gates the actual merge.
			applyVerdict, ok := m.Task("apply-verdict")
			if !ok {
				t.Fatal("apply-verdict task not found")
			}
			wantApplyCapabilities := []string{"github:pr:write", "github:pr:review"}
			if !reflect.DeepEqual(applyVerdict.Capabilities, wantApplyCapabilities) {
				t.Errorf("apply-verdict capabilities = %v, want %v", applyVerdict.Capabilities, wantApplyCapabilities)
			}
			if applyVerdict.Next != "published-verdict" {
				t.Errorf("apply-verdict.next = %q, want published-verdict", applyVerdict.Next)
			}
			publishedVerdict, ok := m.Gate("published-verdict")
			if !ok {
				t.Fatal("published-verdict gate not found")
			}
			if publishedVerdict.Automated == nil ||
				publishedVerdict.Automated.Check != "output-equals" ||
				publishedVerdict.Automated.Params["key"] != "decision" ||
				publishedVerdict.Automated.Params["equals"] != "pass" {
				t.Errorf("published-verdict check = %+v, want decision == pass", publishedVerdict.Automated)
			}
			wantPublishedBranches := map[string]string{"pass": "merge-pr", "fail": TerminalComplete}
			if !reflect.DeepEqual(publishedVerdict.Branches, wantPublishedBranches) {
				t.Errorf("published-verdict branches = %v, want %v", publishedVerdict.Branches, wantPublishedBranches)
			}

			mergePR, ok := m.Task("merge-pr")
			if !ok {
				t.Fatal("merge-pr task not found")
			}
			wantMergeInputs := map[string]string{
				"pullNumber": "selectedNumber",
				"headSha":    "selectedHeadSha",
				"baseSha":    "selectedBaseSha",
			}
			if !reflect.DeepEqual(mergePR.InputsFrom, wantMergeInputs) {
				t.Errorf("merge-pr inputsFrom = %v, want %v", mergePR.InputsFrom, wantMergeInputs)
			}
			if !reflect.DeepEqual(mergePR.Capabilities, []string{"github:pr:merge"}) {
				t.Errorf("merge-pr capabilities = %v, want [github:pr:merge]", mergePR.Capabilities)
			}
			if mergePR.Run == nil || !reflect.DeepEqual(mergePR.Run.Command, []string{"goobers", "merge-pr"}) {
				t.Errorf("merge-pr command = %+v, want [goobers merge-pr]", mergePR.Run)
			}
			if mergePR.Inputs["verdict"] != "pass" || mergePR.Inputs["advisoryMode"] != "false" {
				t.Errorf("merge-pr safety inputs = %v, want verdict=pass advisoryMode=false", mergePR.Inputs)
			}
			if mergePR.Next != "merge-gate" {
				t.Errorf("merge-pr.next = %q, want merge-gate", mergePR.Next)
			}

			mergeGate, ok := m.Gate("merge-gate")
			if !ok {
				t.Fatal("merge-gate not found")
			}
			// Issue #758: merge-gate distinguishes an actual merge from a
			// merge-queue enqueue via "land-outcome", not a plain
			// output-equals(merged==true) — that could only ever say
			// "landed or not", silently conflating "enqueued" with refusal.
			if mergeGate.Automated == nil || mergeGate.Automated.Check != "land-outcome" {
				t.Errorf("merge-gate check = %+v, want land-outcome", mergeGate.Automated)
			}
			wantMergeBranches := map[string]string{"merged": "post-merge", "enqueued": "queue-watch", "fail": TerminalComplete}
			if !reflect.DeepEqual(mergeGate.Branches, wantMergeBranches) {
				t.Errorf("merge-gate branches = %v, want %v", mergeGate.Branches, wantMergeBranches)
			}
			if mergeGate.Branches["fail"] == "apply-verdict" {
				t.Error("merge refusal must not apply the pass verdict label; the PR must remain retryable")
			}

			queueWatch, ok := m.Task("queue-watch")
			if !ok {
				t.Fatal("queue-watch task not found")
			}
			if !reflect.DeepEqual(queueWatch.InputsFrom, map[string]string{"pullNumber": "selectedNumber"}) {
				t.Errorf("queue-watch inputsFrom = %v, want pullNumber=selectedNumber", queueWatch.InputsFrom)
			}
			if queueWatch.Run == nil || !reflect.DeepEqual(queueWatch.Run.Command, []string{"goobers", "merge-queue-poll"}) {
				t.Errorf("queue-watch command = %+v, want [goobers merge-queue-poll]", queueWatch.Run)
			}
			wantQueueWatchCapabilities := []string{"github:pr:merge", "github:issues:write"}
			if !reflect.DeepEqual(queueWatch.Capabilities, wantQueueWatchCapabilities) {
				t.Errorf("queue-watch capabilities = %v, want %v", queueWatch.Capabilities, wantQueueWatchCapabilities)
			}
			if queueWatch.Next != "queue-gate" {
				t.Errorf("queue-watch.next = %q, want queue-gate", queueWatch.Next)
			}

			queueGate, ok := m.Gate("queue-gate")
			if !ok {
				t.Fatal("queue-gate not found")
			}
			if queueGate.Automated == nil || queueGate.Automated.Check != "queue-outcome" {
				t.Errorf("queue-gate check = %+v, want queue-outcome", queueGate.Automated)
			}
			wantQueueBranches := map[string]string{
				"merged": "post-merge", "evicted": TerminalComplete, "timeout": TerminalComplete, "fail": TerminalComplete,
			}
			if !reflect.DeepEqual(queueGate.Branches, wantQueueBranches) {
				t.Errorf("queue-gate branches = %v, want %v", queueGate.Branches, wantQueueBranches)
			}

			postMerge, ok := m.Task("post-merge")
			if !ok {
				t.Fatal("post-merge task not found")
			}
			if !reflect.DeepEqual(postMerge.InputsFrom, map[string]string{"pullNumber": "selectedNumber"}) {
				t.Errorf("post-merge inputsFrom = %v, want pullNumber=selectedNumber", postMerge.InputsFrom)
			}
			if postMerge.Run == nil || !reflect.DeepEqual(postMerge.Run.Command, []string{"goobers", "post-merge"}) {
				t.Errorf("post-merge command = %+v, want [goobers post-merge]", postMerge.Run)
			}
			wantPostMergeCapabilities := []string{"github:pr:write", "github:issues:write"}
			if !reflect.DeepEqual(postMerge.Capabilities, wantPostMergeCapabilities) {
				t.Errorf("post-merge capabilities = %v, want %v", postMerge.Capabilities, wantPostMergeCapabilities)
			}

			// A shell stage's Outputs are harvested ONLY from a declared
			// result file (internal/executor/shell.go: the whole harvest
			// lives inside `if resultFile != ""`, and its own comment notes
			// "a stage with no declared resultFile has result.Outputs empty
			// here"). expectedOutputs is documentation, not enforcement —
			// nothing cross-checks the two.
			//
			// elect-lander declared five expectedOutputs and no resultFile,
			// so it emitted NONE of them while still exiting 0. `elected`
			// never reached elect-gate, whose output-equals check read the
			// missing key as false and routed EVERY needs-changes review
			// down the fail branch into apply-verdict — where the equally
			// missing reviewDigest failed inputsFrom and killed the run.
			// 100% of needs-changes cycles died there, which severed the
			// only path from merge-review to pr-remediation and stalled the
			// instance for three days.
			//
			// Asserted for every shell stage, not just elect-lander: the
			// defect is silent by construction, so the guard has to be a
			// property of the workflow rather than a spot check.
			for _, task := range w.Spec.Tasks {
				if task.Run == nil || len(task.ExpectedOutputs) == 0 {
					continue
				}
				if task.Inputs["resultFile"] == "" {
					t.Errorf("stage %q declares expectedOutputs %v but no resultFile input — it will emit no outputs at all, silently", task.Name, task.ExpectedOutputs)
				}
			}
		})
	}
}
