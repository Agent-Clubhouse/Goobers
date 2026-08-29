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
		name         string
		root         string
		reviewerPath string
		reviewerName string
	}{
		{
			name:         "reference-workflows",
			root:         filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers"),
			reviewerPath: filepath.Join("goobers", "reviewer", "goober.yaml"),
			reviewerName: "reviewer",
		},
		{
			name:         "acme-web",
			root:         filepath.Join("..", "..", "config-examples", "gaggles", "acme-web"),
			reviewerPath: filepath.Join("goobers", "reviewer", "goober.yaml"),
			reviewerName: "reviewer",
		},
		{
			name:         "acme-web-claude",
			root:         filepath.Join("..", "..", "config-examples", "gaggles", "acme-web-claude"),
			reviewerPath: filepath.Join("goobers", "claude-reviewer", "goober.yaml"),
			reviewerName: "claude-reviewer",
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

			raw, err = os.ReadFile(filepath.Join(tt.root, tt.reviewerPath))
			if err != nil {
				t.Fatalf("read reviewer: %v", err)
			}
			var reviewer apiv1.Goober
			if err := yaml.Unmarshal(raw, &reviewer); err != nil {
				t.Fatalf("unmarshal reviewer: %v", err)
			}
			registered := false
			for _, workflowName := range reviewer.Spec.Workflows {
				if workflowName == w.Name {
					registered = true
					break
				}
			}
			if !registered {
				t.Error("reviewer is not registered for merge-review")
			}

			m, err := compileAcknowledged(
				Definition{Name: w.Name, Version: 1, Spec: w.Spec},
				WithGoobers(map[string]apiv1.GooberSpec{tt.reviewerName: reviewer.Spec}),
				WithKnownChecks([]string{"output-equals", "land-outcome", "queue-outcome"}))

			if err != nil {
				t.Fatalf("compile workflow: %v", err)
			}
			if w.Spec.Start != "reconcile-post-merge" {
				t.Fatalf("workflow start = %q, want reconcile-post-merge", w.Spec.Start)
			}
			reconcile, ok := m.Task("reconcile-post-merge")
			if !ok {
				t.Fatal("reconcile-post-merge task not found")
			}
			if reconcile.Run == nil || !reflect.DeepEqual(reconcile.Run.Command, []string{"goobers", "reconcile-post-merge"}) {
				t.Errorf("reconcile-post-merge command = %+v, want [goobers reconcile-post-merge]", reconcile.Run)
			}
			if reconcile.Inputs["resultFile"] != "reconcile-post-merge-result.json" {
				t.Errorf("reconcile-post-merge resultFile = %q, want reconcile-post-merge-result.json", reconcile.Inputs["resultFile"])
			}
			if reconcile.Next != "pr-select" {
				t.Errorf("reconcile-post-merge.next = %q, want pr-select", reconcile.Next)
			}
			if !reconcile.ContinueOnError {
				t.Error("reconcile-post-merge must not block the primary review path when its bounded sweep fails")
			}
			wantReconcileCapabilities := []string{"github:pr:write", "github:issues:write", "github:branch:delete"}
			if !reflect.DeepEqual(reconcile.Capabilities, wantReconcileCapabilities) {
				t.Errorf("reconcile-post-merge capabilities = %v, want %v", reconcile.Capabilities, wantReconcileCapabilities)
			}
			wantReconcilePolicyActions := []string{
				"close-issues",
				"fan-out-remediation",
				"unpark-resolved-siblings",
				"clear-healed-escalations",
				"clear-healed-demotions",
				"delete-branch",
			}
			if !reflect.DeepEqual(reconcile.PolicyActions, wantReconcilePolicyActions) {
				t.Errorf("reconcile-post-merge policyActions = %v, want %v", reconcile.PolicyActions, wantReconcilePolicyActions)
			}

			prSelect, ok := m.Task("pr-select")
			if !ok {
				t.Fatal("pr-select task not found")
			}
			wantHeadPrefixes := "goobers/implementation/,goobers/docs-updater/"
			if tt.name == "reference-workflows" {
				wantHeadPrefixes += ",goobers/tutor/"
			}
			if got := prSelect.Inputs["headPrefixes"]; got != wantHeadPrefixes {
				t.Errorf("pr-select headPrefixes = %q, want %q", got, wantHeadPrefixes)
			}
			if got := prSelect.Inputs["authorScope"]; got != "any" {
				t.Errorf("pr-select authorScope = %q, want any", got)
			}
			if want := []string{"number", "head", "base", "advisoryMode"}; !reflect.DeepEqual(prSelect.ExpectedOutputs, want) {
				t.Errorf("pr-select expectedOutputs = %v, want %v", prSelect.ExpectedOutputs, want)
			}
			if _, legacy := prSelect.Inputs["headPrefix"]; legacy {
				t.Error("pr-select retained legacy headPrefix input")
			}
			if want := []string{"flag-foundation-coupling"}; !reflect.DeepEqual(prSelect.PolicyActions, want) {
				t.Errorf("pr-select policyActions = %v, want %v", prSelect.PolicyActions, want)
			}
			gatherSiblings, ok := m.Task("gather-sibling-context")
			if !ok {
				t.Fatal("gather-sibling-context task not found")
			}
			if want := []string{"flag-scope-drift", "route-verdict"}; !reflect.DeepEqual(gatherSiblings.PolicyActions, want) {
				t.Errorf("gather-sibling-context policyActions = %v, want %v", gatherSiblings.PolicyActions, want)
			}
			if gatherSiblings.Inputs["authorScope"] != "any" ||
				gatherSiblings.Inputs["headPrefixes"] != wantHeadPrefixes ||
				gatherSiblings.InputsFrom["advisoryMode"] != "advisoryMode" {
				t.Errorf("gather-sibling-context advisory wiring = inputs %v inputsFrom %v", gatherSiblings.Inputs, gatherSiblings.InputsFrom)
			}
			if gatherSiblings.Next != "review" {
				t.Errorf("gather-sibling-context.next = %q, want review so oversized PRs are still reviewed", gatherSiblings.Next)
			}

			stalenessGate, ok := m.Gate("issue-staleness-gate")
			if !ok {
				t.Fatal("issue-staleness-gate not found")
			}
			if stalenessGate.Automated == nil ||
				stalenessGate.Automated.Check != "output-equals" ||
				stalenessGate.Automated.Params["key"] != "issueStale" ||
				stalenessGate.Automated.Params["equals"] != "false" {
				t.Errorf("issue-staleness-gate check = %+v, want issueStale == false", stalenessGate.Automated)
			}
			if want := map[string]string{"pass": "gather-sibling-context", "fail": TargetAbort}; !reflect.DeepEqual(stalenessGate.Branches, want) {
				t.Errorf("issue-staleness-gate branches = %v, want %v", stalenessGate.Branches, want)
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
			// off to elect-gate. Both outcomes publish through apply-verdict;
			// crowned landers receive a derived pass, while the others park.
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
			if electLander.InputsFrom["advisoryMode"] != "advisoryMode" {
				t.Errorf("elect-lander advisoryMode input = %q, want advisoryMode", electLander.InputsFrom["advisoryMode"])
			}
			if electLander.InputsFrom["scopeGateParked"] != "scopeGateParked" {
				t.Errorf("elect-lander scopeGateParked input = %q, want scopeGateParked", electLander.InputsFrom["scopeGateParked"])
			}
			if !containsString(electLander.ExpectedOutputs, "scopeGateParked") {
				t.Errorf("elect-lander expectedOutputs = %v, want scopeGateParked pass-through", electLander.ExpectedOutputs)
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
			wantApplyCapabilities := []string{"provider:pr:write", "github:pr:review"}
			if !reflect.DeepEqual(applyVerdict.Capabilities, wantApplyCapabilities) {
				t.Errorf("apply-verdict capabilities = %v, want %v", applyVerdict.Capabilities, wantApplyCapabilities)
			}
			if applyVerdict.Next != "advisory-verdict" {
				t.Errorf("apply-verdict.next = %q, want advisory-verdict", applyVerdict.Next)
			}
			wantApplyInputs := map[string]string{
				"selectedNumber":      "selectedNumber",
				"selectedHeadSha":     "selectedHeadSha",
				"selectedBaseSha":     "selectedBaseSha",
				"advisoryMode":        "advisoryMode",
				"reviewDigest":        "reviewDigest",
				"overlappingSiblings": "overlappingSiblingsCsv",
				"scopeGateParked":     "scopeGateParked",
			}
			if !reflect.DeepEqual(applyVerdict.InputsFrom, wantApplyInputs) {
				t.Errorf("apply-verdict inputsFrom = %v, want %v", applyVerdict.InputsFrom, wantApplyInputs)
			}
			if !containsString(applyVerdict.ExpectedOutputs, "scopeGateParked") {
				t.Errorf("apply-verdict expectedOutputs = %v, want scopeGateParked pass-through", applyVerdict.ExpectedOutputs)
			}
			advisoryVerdict, ok := m.Gate("advisory-verdict")
			if !ok {
				t.Fatal("advisory-verdict gate not found")
			}
			if advisoryVerdict.Automated == nil ||
				advisoryVerdict.Automated.Check != "output-equals" ||
				advisoryVerdict.Automated.Params["key"] != "advisoryMode" ||
				advisoryVerdict.Automated.Params["equals"] != "true" {
				t.Errorf("advisory-verdict check = %+v, want advisoryMode == true", advisoryVerdict.Automated)
			}
			wantAdvisoryBranches := map[string]string{"pass": TerminalComplete, "fail": "published-verdict"}
			if !reflect.DeepEqual(advisoryVerdict.Branches, wantAdvisoryBranches) {
				t.Errorf("advisory-verdict branches = %v, want %v", advisoryVerdict.Branches, wantAdvisoryBranches)
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
			wantPublishedBranches := map[string]string{"pass": "scope-gate", "fail": TerminalComplete}
			if !reflect.DeepEqual(publishedVerdict.Branches, wantPublishedBranches) {
				t.Errorf("published-verdict branches = %v, want %v", publishedVerdict.Branches, wantPublishedBranches)
			}
			scopeGate, ok := m.Gate("scope-gate")
			if !ok {
				t.Fatal("scope-gate gate not found")
			}
			if scopeGate.Automated == nil ||
				scopeGate.Automated.Check != "output-equals" ||
				scopeGate.Automated.Params["key"] != "scopeGateParked" ||
				scopeGate.Automated.Params["equals"] != "false" {
				t.Errorf("scope-gate check = %+v, want scopeGateParked == false", scopeGate.Automated)
			}
			wantScopeBranches := map[string]string{"pass": "merge-pr", "fail": TerminalComplete}
			if !reflect.DeepEqual(scopeGate.Branches, wantScopeBranches) {
				t.Errorf("scope-gate branches = %v, want %v", scopeGate.Branches, wantScopeBranches)
			}

			mergePR, ok := m.Task("merge-pr")
			if !ok {
				t.Fatal("merge-pr task not found")
			}
			wantMergeInputs := map[string]string{
				"pullNumber":    "selectedNumber",
				"headSha":       "selectedHeadSha",
				"baseSha":       "selectedBaseSha",
				"verdictAuthor": "verdictAuthor",
			}
			if !reflect.DeepEqual(mergePR.InputsFrom, wantMergeInputs) {
				t.Errorf("merge-pr inputsFrom = %v, want %v", mergePR.InputsFrom, wantMergeInputs)
			}
			if !reflect.DeepEqual(mergePR.Capabilities, []string{"github:pr:merge", "github:branch:delete"}) {
				t.Errorf("merge-pr capabilities = %v, want [github:pr:merge github:branch:delete]", mergePR.Capabilities)
			}
			if mergePR.Run == nil || !reflect.DeepEqual(mergePR.Run.Command, []string{"goobers", "merge-pr"}) {
				t.Errorf("merge-pr command = %+v, want [goobers merge-pr]", mergePR.Run)
			}
			if mergePR.Inputs["verdict"] != "pass" || mergePR.Inputs["advisoryMode"] != "false" {
				t.Errorf("merge-pr safety inputs = %v, want verdict=pass advisoryMode=false", mergePR.Inputs)
			}
			if mergePR.Next != "merge-opt-out-gate" {
				t.Errorf("merge-pr.next = %q, want merge-opt-out-gate", mergePR.Next)
			}

			mergeOptOutGate, ok := m.Gate("merge-opt-out-gate")
			if !ok {
				t.Fatal("merge-opt-out-gate not found")
			}
			if mergeOptOutGate.Automated == nil ||
				mergeOptOutGate.Automated.Check != "output-equals" ||
				mergeOptOutGate.Automated.Params["key"] != "optedOut" ||
				mergeOptOutGate.Automated.Params["equals"] != "true" {
				t.Errorf("merge-opt-out-gate check = %+v, want optedOut == true", mergeOptOutGate.Automated)
			}
			wantMergeOptOutBranches := map[string]string{"pass": TerminalComplete, "fail": "merge-gate"}
			if !reflect.DeepEqual(mergeOptOutGate.Branches, wantMergeOptOutBranches) {
				t.Errorf("merge-opt-out-gate branches = %v, want %v", mergeOptOutGate.Branches, wantMergeOptOutBranches)
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
			// #950: a merge refusal now routes to record-merge-refusal (which
			// counts consecutive refusals at an unchanged head and eventually
			// demotes a stuck lander) rather than straight to a terminal.
			wantMergeBranches := map[string]string{"merged": "post-merge", "enqueued": "queue-watch", "fail": "record-merge-refusal"}
			if !reflect.DeepEqual(mergeGate.Branches, wantMergeBranches) {
				t.Errorf("merge-gate branches = %v, want %v", mergeGate.Branches, wantMergeBranches)
			}
			if mergeGate.Branches["fail"] == "apply-verdict" {
				t.Error("merge refusal must not apply the pass verdict label; the PR must remain retryable")
			}
			// record-merge-refusal is terminal (its own demotion side effects are
			// the durable output) and must not itself apply the pass verdict.
			recordRefusal, ok := m.Task("record-merge-refusal")
			if !ok {
				t.Fatal("record-merge-refusal task not found")
			}
			if recordRefusal.Next != TerminalComplete {
				t.Errorf("record-merge-refusal.next = %q, want terminal", recordRefusal.Next)
			}
			wantRefusalCaps := []string{"github:pr:write", "github:issues:write"}
			if !reflect.DeepEqual(recordRefusal.Capabilities, wantRefusalCaps) {
				t.Errorf("record-merge-refusal capabilities = %v, want %v", recordRefusal.Capabilities, wantRefusalCaps)
			}
			wantRefusalInputs := map[string]string{
				"selectedNumber":  "selectedNumber",
				"reason":          "reason",
				"selectedHeadSha": "selectedHeadSha",
			}
			if !reflect.DeepEqual(recordRefusal.InputsFrom, wantRefusalInputs) {
				t.Errorf("record-merge-refusal inputsFrom = %v, want %v", recordRefusal.InputsFrom, wantRefusalInputs)
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
			wantQueueWatchCapabilities := []string{"github:pr:merge", "github:issues:write", "github:branch:delete"}
			if !reflect.DeepEqual(queueWatch.Capabilities, wantQueueWatchCapabilities) {
				t.Errorf("queue-watch capabilities = %v, want %v", queueWatch.Capabilities, wantQueueWatchCapabilities)
			}
			if queueWatch.Next != "queue-opt-out-gate" {
				t.Errorf("queue-watch.next = %q, want queue-opt-out-gate", queueWatch.Next)
			}

			queueOptOutGate, ok := m.Gate("queue-opt-out-gate")
			if !ok {
				t.Fatal("queue-opt-out-gate not found")
			}
			if queueOptOutGate.Automated == nil ||
				queueOptOutGate.Automated.Check != "output-equals" ||
				queueOptOutGate.Automated.Params["key"] != "queueOutcome" ||
				queueOptOutGate.Automated.Params["equals"] != "skipped" {
				t.Errorf("queue-opt-out-gate check = %+v, want queueOutcome == skipped", queueOptOutGate.Automated)
			}
			wantQueueOptOutBranches := map[string]string{"pass": TerminalComplete, "fail": "queue-gate"}
			if !reflect.DeepEqual(queueOptOutGate.Branches, wantQueueOptOutBranches) {
				t.Errorf("queue-opt-out-gate branches = %v, want %v", queueOptOutGate.Branches, wantQueueOptOutBranches)
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
			if postMerge.Inputs["resultFile"] != "post-merge-result.json" {
				t.Errorf("post-merge resultFile = %q, want post-merge-result.json", postMerge.Inputs["resultFile"])
			}
			wantPostMergeCapabilities := []string{"github:pr:write", "github:issues:write"}
			if !reflect.DeepEqual(postMerge.Capabilities, wantPostMergeCapabilities) {
				t.Errorf("post-merge capabilities = %v, want %v", postMerge.Capabilities, wantPostMergeCapabilities)
			}
			wantPostMergePolicyActions := []string{
				"close-issues",
				"fan-out-remediation",
				"unpark-resolved-siblings",
				"clear-healed-escalations",
				"clear-healed-demotions",
			}
			if !reflect.DeepEqual(postMerge.PolicyActions, wantPostMergePolicyActions) {
				t.Errorf("post-merge policyActions = %v, want %v", postMerge.PolicyActions, wantPostMergePolicyActions)
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
