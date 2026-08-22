package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestImplementationEscalatingBranchesRunIssueBookkeeping guards #929 across
// EVERY shipped copy of the implementation workflow.
//
// A gate branch may reach an escalating terminal in one of two ways, and they
// are not equivalent even though the run's terminal phase is identical:
//
//	timeout: "@escalate"      # terminal immediately
//	timeout: park-escalated   # ... whose own next IS "@escalate"
//
// Only the second runs park-escalated, and park-escalated is what performs the
// issue-side bookkeeping: `goobers issue-close-out --status needs-remediation`
// (#2028 — every park-escalated cause is a mechanical failure, not a
// decision), which clears goobers:ready, releases goobers:claimed, and
// applies goobers:needs-remediation. Routing at the reserved terminal
// directly skips it.
//
// The consequence is silent and unrecoverable, which is why it needs a test
// rather than review attention. weekend_10 (2026-07-19) hit two real ci-poll
// timeouts; both runs terminated with the correct `escalated` phase, so every
// escalation surface looked right, while issues #515 and #444 were left
// carrying goobers:ready + goobers:claimed and never got parked at all.
// Still-claimed means query-backlog will not re-offer them; no park label
// means no human search finds them. They did not fail — they silently left the
// workable backlog with no signal pointing back at them.
//
// Asserted over both copies deliberately: the defect was present identically in
// reference-workflows/ and config-examples/, so a test pinning only one of them would have
// let the other drift right back. (The live instance keeps a third,
// hand-maintained copy that no test can reach — see each file's INTENTIONAL
// LIVE DIVERGENCE header — so that one must be synced by hand.)
func TestImplementationEscalatingBranchesRunIssueBookkeeping(t *testing.T) {
	for _, root := range []string{
		filepath.Join("..", "..", "config-examples", "gaggles", "acme-web"),
		filepath.Join("..", "..", "reference-workflows", "gaggles", "goobers"),
	} {
		t.Run(root, func(t *testing.T) {
			var w apiv1.Workflow
			raw, err := os.ReadFile(filepath.Join(root, "workflows", "implementation.yaml"))
			if err != nil {
				t.Fatalf("read workflow: %v", err)
			}
			if err := yaml.Unmarshal(raw, &w); err != nil {
				t.Fatalf("unmarshal workflow: %v", err)
			}
			m, err := compileAcknowledged(Definition{Name: w.Name, Version: 1, Spec: w.Spec})
			if err != nil {
				t.Fatalf("compile: %v", err)
			}

			// park-escalated must still reach the escalating terminal itself,
			// so routing through it does not change the run's terminal phase
			// (`goobers run` exit 3, the read API's escalationCause, and
			// `goobers trace` all key on PhaseEscalated).
			park, ok := m.Task("park-escalated")
			if !ok {
				t.Fatal("park-escalated task not found")
			}
			if park.Next != TargetEscalate {
				t.Errorf("park-escalated.next = %q, want %q — routing a branch through it must not change the run's terminal phase", park.Next, TargetEscalate)
			}
			if park.Inputs["status"] != "needs-remediation" {
				t.Errorf("park-escalated inputs = %v, want status=needs-remediation — this is the bookkeeping the branch is routed here for", park.Inputs)
			}

			mechanicalRoutes := map[string][]string{
				// review's escalation branch receives empty diffs, repass
				// exhaustion, identical diffs, and non-retryable failures.
				"review":     {BranchEscalate},
				"local-gate": {BranchEscalate},
			}
			for gateName, outcomes := range mechanicalRoutes {
				g, ok := m.Gate(gateName)
				if !ok {
					t.Errorf("%s not found", gateName)
					continue
				}
				for _, outcome := range outcomes {
					target, ok := BranchTarget(g, outcome)
					if !ok {
						t.Errorf("%s has no %q branch", gateName, outcome)
						continue
					}
					if target != "park-escalated" {
						t.Errorf("%s %s branch = %q, want park-escalated; execution stalls must not route to needs-human", gateName, outcome, target)
					}
				}
			}
			localGate, _ := m.Gate("local-gate")
			if target, ok := BranchTarget(localGate, "infra"); !ok || target != "local-ci" {
				t.Errorf("local-gate infra branch = %q,%v, want local-ci,true", target, ok)
			}
			ciGate, ok := m.Gate("ci-gate")
			if !ok {
				t.Error("ci-gate not found")
			} else if target, found := BranchTarget(ciGate, "timeout"); !found || target != "ci-poll" {
				t.Errorf("ci-gate timeout branch = %q,%v, want ci-poll,true; pending checks must continue without terminal parking", target, found)
			}

			human, ok := m.Task("park-needs-human")
			if !ok {
				t.Fatal("park-needs-human task not found")
			}
			if human.Inputs["status"] != "needs-human" {
				t.Errorf("park-needs-human inputs = %v, want status=needs-human", human.Inputs)
			}
			review, _ := m.Gate("review")
			if target, ok := BranchTarget(review, "fail"); !ok || target != "park-needs-human" {
				t.Errorf("review fail branch = %q,%v, want park-needs-human,true", target, ok)
			}
		})
	}
}
