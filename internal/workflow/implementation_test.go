package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestImplementationWorkflowCompiles compiles the shipped implementation
// workflow (config-examples/, the flagship V0 workload — issue #27) against
// its implementer and reviewer goobers and pins a golden digest.
func TestImplementationWorkflowCompiles(t *testing.T) {
	root := filepath.Join("..", "..", "config-examples", "gaggles", "acme-web")

	var w apiv1.Workflow
	raw, err := os.ReadFile(filepath.Join(root, "workflows", "implementation.yaml"))
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal workflow: %v", err)
	}

	goobers := map[string]apiv1.GooberSpec{}
	for _, name := range []string{"implementer", "reviewer"} {
		var g apiv1.Goober
		raw, err := os.ReadFile(filepath.Join(root, "goobers", name, "goober.yaml"))
		if err != nil {
			t.Fatalf("read %s goober: %v", name, err)
		}
		if err := yaml.Unmarshal(raw, &g); err != nil {
			t.Fatalf("unmarshal %s goober: %v", name, err)
		}
		goobers[g.Name] = g.Spec
	}

	def := Definition{Name: w.Name, Version: 1, Spec: w.Spec}
	m, err := Compile(def, WithGoobers(goobers))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Structural shape: query-backlog -> implement -> review(gate) ->
	// {local-ci on pass, implement on needs-changes, park on fail/escalate}.
	if w.Spec.Start != "query-backlog" {
		t.Errorf("start = %q, want query-backlog", w.Spec.Start)
	}
	implement, ok := m.Task("implement")
	if !ok {
		t.Fatal("implement task not found")
	}
	if implement.Next != "review" {
		t.Errorf("implement.next = %q, want review", implement.Next)
	}
	if implement.Goober != "implementer" {
		t.Errorf("implement.goober = %q, want implementer", implement.Goober)
	}
	// #724: the implement stage salvages a viable committed diff on session
	// timeout rather than discarding actively-progressed work.
	if implement.OnTimeout != apiv1.TaskOnTimeoutSalvage {
		t.Errorf("implement.onTimeout = %q, want %q", implement.OnTimeout, apiv1.TaskOnTimeoutSalvage)
	}
	localCI, ok := m.Task("local-ci")
	if !ok {
		t.Fatal("local-ci task not found")
	}
	if localCI.Run == nil || !localCI.Run.SyncBase {
		t.Error("local-ci.run.syncBase = false, want true")
	}
	review, ok := m.Gate("review")
	if !ok {
		t.Fatal("review gate not found")
	}
	if review.Agentic == nil || review.Agentic.Goober != "reviewer" {
		t.Errorf("review.agentic.goober = %+v, want reviewer", review.Agentic)
	}
	wantBranches := map[string]string{
		"pass":          "local-ci",
		"needs-changes": "implement",
		"fail":          "park-needs-human",
		BranchEscalate:  "park-escalated",
	}
	for outcome, want := range wantBranches {
		got, ok := BranchTarget(review, outcome)
		if !ok || got != want {
			t.Errorf("review branch %q = %q,%v; want %q,true", outcome, got, ok, want)
		}
	}

	// The bounded repass loop closes back to implement from two independent
	// gates (review:needs-changes, ci-gate:fail) — both must resolve.
	ciGate, ok := m.Gate("ci-gate")
	if !ok {
		t.Fatal("ci-gate not found")
	}
	if target, ok := BranchTarget(ciGate, "fail"); !ok || target != "implement" {
		t.Errorf("ci-gate fail branch = %q,%v; want implement,true", target, ok)
	}
	if target, ok := BranchTarget(ciGate, "pass"); !ok || target != "close-out" {
		t.Errorf("ci-gate pass branch = %q,%v; want close-out,true", target, ok)
	}
	for _, gateName := range []string{"local-gate", "ci-gate"} {
		g, ok := m.Gate(gateName)
		if !ok {
			t.Fatalf("%s not found", gateName)
		}
		if target, ok := BranchTarget(g, BranchEscalate); !ok || target != "park-escalated" {
			t.Errorf("%s escalation branch = %q,%v; want park-escalated,true", gateName, target, ok)
		}
	}

	closeOut, ok := m.Task("close-out")
	if !ok {
		t.Fatal("close-out task not found")
	}
	if closeOut.Next != "" {
		t.Errorf("close-out.next = %q, want terminal", closeOut.Next)
	}
	park, ok := m.Task("park-needs-human")
	if !ok {
		t.Fatal("park-needs-human task not found")
	}
	if park.Next != TargetAbort {
		t.Errorf("park-needs-human.next = %q, want %q", park.Next, TargetAbort)
	}
	if park.Inputs["status"] != "needs-human" {
		t.Errorf("park-needs-human inputs = %v, want status=needs-human", park.Inputs)
	}

	// #947: open-pr re-checks the claimed issue is still open before opening,
	// routing through open-pr-gate (opened=true -> ci-poll, opened=false ->
	// @abort) so an issue closed mid-flight never produces a stale PR.
	openPR, ok := m.Task("open-pr")
	if !ok {
		t.Fatal("open-pr task not found")
	}
	if openPR.Next != "open-pr-gate" {
		t.Errorf("open-pr.next = %q, want open-pr-gate", openPR.Next)
	}
	openPRGate, ok := m.Gate("open-pr-gate")
	if !ok {
		t.Fatal("open-pr-gate not found")
	}
	if target, ok := BranchTarget(openPRGate, "pass"); !ok || target != "ci-poll" {
		t.Errorf("open-pr-gate pass branch = %q,%v; want ci-poll,true", target, ok)
	}
	if target, ok := BranchTarget(openPRGate, "fail"); !ok || target != TargetAbort {
		t.Errorf("open-pr-gate fail branch = %q,%v; want %q,true", target, ok, TargetAbort)
	}

	// Capability grants match issue #27's scope, split by least privilege:
	// implementer=[repo:push], reviewer=[] (pure evaluation, no write).
	if len(goobers["implementer"].Capabilities) != 1 || goobers["implementer"].Capabilities[0] != "repo:push" {
		t.Errorf("implementer capabilities = %v, want exactly [repo:push]", goobers["implementer"].Capabilities)
	}
	if len(goobers["reviewer"].Capabilities) != 0 {
		t.Errorf("reviewer capabilities = %v, want none", goobers["reviewer"].Capabilities)
	}

	// #239: ci-gate gained a "timeout" branch (routes a ci-poll timeout to
	// @escalate instead of the "fail" branch's implement repass).
	// #237: a deterministic push-branch stage was inserted between
	// local-gate and open-pr (the implementer commits but no longer pushes).
	// #361/#355: query-backlog gained excludeLabels (goobers/status:in-review)
	// and close-out gained inputs.status="in-review" — the issue no longer
	// closes on PR-open; only `goobers post-merge` (merge-review's opt-in
	// stage, #360/#568) advances it to done at the actual merge event.
	// #724: implement stage gained onTimeout=salvage and its goal was reworded
	// to hand the full `make ci`/`-race` run to the deterministic local-ci
	// stage (bounding the session to think-time, not test wall-clock).
	// #813: local-ci gained run.syncBase so an in-flight implementation branch
	// incorporates build/test behavior fixes fetched from main before testing.
	// local-ci previously carried inputs.serializeGroup=local-ci to throttle
	// concurrent `make ci` runs; that mitigation targeted a disk-I/O contention
	// theory that the #845 post-mortem falsified — the real cause was terminal
	// job control (SIGTTOU), fixed by Setsid (#846/#850). The serialize input
	// was removed so local-ci runs fully parallel again.
	// #929: ci-gate's timeout branch now routes through park-escalated rather
	// than straight at "@escalate". The run's terminal phase is unchanged, but
	// the issue-side bookkeeping (clear ready, release claimed, apply
	// needs-human) only runs if that stage does — see
	// TestImplementationEscalatingBranchesRunIssueBookkeeping.
	// #947: open-pr now emits an `opened` output and routes through the new
	// open-pr-gate (opened=false -> @abort) so an issue closed after it was
	// claimed does not still produce a PR — a re-check immediately before
	// opening, since the claim was only validated once at query-backlog.
	const wantDigest = "sha256:9649a98cc9a58d673cf124d72d128dcf8c669459bde08a5a5ec00aa4c4ab1b4b"
	if m.Digest() != wantDigest {
		t.Logf("implementation digest = %s", m.Digest())
		t.Errorf("digest drift for implementation:\n got  %s\n want %s\n(update wantDigest if the change is intended)", m.Digest(), wantDigest)
	}

	m2, err := Compile(def, WithGoobers(goobers))
	if err != nil {
		t.Fatalf("recompile: %v", err)
	}
	if m.Digest() != m2.Digest() {
		t.Errorf("digest not stable across compiles: %s vs %s", m.Digest(), m2.Digest())
	}
}

// TestImplementationWorkflowRejectsUngrantedCapability guards capability
// admission for this workload: an implement stage declaring a capability the
// implementer goober doesn't hold must fail closed (SEC-042).
func TestImplementationWorkflowRejectsUngrantedCapability(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "acme-web",
		Start:  "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskAgentic, Goober: "implementer", Goal: "implement",
				Capabilities: []string{"repo:push", "github:pr:write"}, // pr:write not granted below
			},
		},
	}
	goobers := map[string]apiv1.GooberSpec{
		"implementer": {Role: "implementer", Harness: apiv1.HarnessCopilot, Capabilities: []string{"repo:push"}},
	}
	_, err := Compile(Definition{Name: "implementation", Version: 1, Spec: spec}, WithGoobers(goobers))
	if err == nil {
		t.Fatal("expected compile to reject an ungranted github:pr:write capability, got nil error")
	}
}
