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
// issue-side bookkeeping: `goobers issue-close-out --status needs-human`, which
// clears goobers:ready, releases goobers:claimed, and applies
// goobers:needs-human. Routing at the reserved terminal directly skips it.
//
// The consequence is silent and unrecoverable, which is why it needs a test
// rather than review attention. weekend_10 (2026-07-19) hit two real ci-poll
// timeouts; both runs terminated with the correct `escalated` phase, so every
// escalation surface looked right, while issues #515 and #444 were left
// carrying goobers:ready + goobers:claimed and never got goobers:needs-human.
// Still-claimed means query-backlog will not re-offer them; no needs-human
// means no human search finds them. They did not fail — they silently left the
// workable backlog with no signal pointing back at them.
//
// Asserted over both copies deliberately: the defect was present identically in
// selfhost/ and config-examples/, so a test pinning only one of them would have
// let the other drift right back. (The live instance keeps a third,
// hand-maintained copy that no test can reach — see each file's INTENTIONAL
// LIVE DIVERGENCE header — so that one must be synced by hand.)
func TestImplementationEscalatingBranchesRunIssueBookkeeping(t *testing.T) {
	for _, root := range []string{
		filepath.Join("..", "..", "config-examples", "gaggles", "acme-web"),
		filepath.Join("..", "..", "selfhost", "gaggles", "goobers"),
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
			m, err := Compile(Definition{Name: w.Name, Version: 1, Spec: w.Spec})
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
			if park.Inputs["status"] != "needs-human" {
				t.Errorf("park-escalated inputs = %v, want status=needs-human — this is the bookkeeping the branch is routed here for", park.Inputs)
			}

			// Every branch of ci-gate that ends the run in escalation must go
			// through park-escalated first. `timeout` is the one #929 missed;
			// `escalate` is asserted alongside it so the pair cannot diverge
			// again in the other direction.
			ciGate, ok := m.Gate("ci-gate")
			if !ok {
				t.Fatal("ci-gate not found")
			}
			for _, outcome := range []string{"timeout", BranchEscalate} {
				target, ok := BranchTarget(ciGate, outcome)
				if !ok {
					t.Errorf("ci-gate has no %q branch", outcome)
					continue
				}
				if IsReservedTarget(target) {
					t.Errorf("ci-gate %s branch = %q: routes straight at a reserved terminal, skipping park-escalated. "+
						"The run phase is right but the driving issue is orphaned — still goobers:ready, still "+
						"goobers:claimed, never goobers:needs-human (#929). Route it through park-escalated, whose "+
						"own next is already %q.", outcome, target, TargetEscalate)
					continue
				}
				if target != "park-escalated" {
					t.Errorf("ci-gate %s branch = %q, want park-escalated", outcome, target)
				}
			}
		})
	}
}
