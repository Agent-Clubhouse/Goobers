package main

import (
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func blockedFinding(on ...int) apiv1.Finding {
	return apiv1.Finding{
		Severity:    apiv1.SeverityInfo,
		Message:     "waiting on a sibling",
		Class:       apiv1.FindingCrossPRBlocked,
		BlockingPRs: on,
	}
}

// TestElectedLanderPass pins the reframed meaning of election: being crowned
// means "those siblings no longer block you", NOT "merge regardless of review".
//
// Once that is stated, the pass is DERIVED rather than granted. Every finding
// was a pure ordering ask (so the PR is individually fine) and this PR is the
// one whose turn it is, therefore nothing is left blocking it. The distinction
// matters because it is what keeps merge-pr's safety conjuncts honest: the
// crowned lander now reaches merge-pr through the ordinary
// apply-verdict -> published-verdict path carrying a real, SHA-pinned pass
// verdict comment, instead of bypassing review publication entirely and
// satisfying merge-pr's "was this reviewed favorably" check from a hardcoded
// constant.
func TestElectedLanderPass(t *testing.T) {
	tests := []struct {
		name     string
		number   int
		verdict  apiv1.Verdict
		wantPass bool
	}{
		{
			// fifo: lowest number in the cluster goes first.
			name:   "lowest in cluster with only ordering findings is elected",
			number: 10,
			verdict: apiv1.Verdict{
				Decision: apiv1.VerdictNeedsChanges,
				Findings: []apiv1.Finding{blockedFinding(11, 12)},
			},
			wantPass: true,
		},
		{
			name:   "not lowest is not elected",
			number: 12,
			verdict: apiv1.Verdict{
				Decision: apiv1.VerdictNeedsChanges,
				Findings: []apiv1.Finding{blockedFinding(10, 11)},
			},
			wantPass: false,
		},
		{
			// The load-bearing safety property. A real defect alongside the
			// ordering asks must never be resolved away by election — the PR
			// genuinely needs work, and going first does not fix it.
			name:   "a real defect alongside ordering findings is never elected",
			number: 10,
			verdict: apiv1.Verdict{
				Decision: apiv1.VerdictNeedsChanges,
				Findings: []apiv1.Finding{
					blockedFinding(11),
					{Severity: apiv1.SeverityError, Message: "nil deref", Class: apiv1.FindingSubstantive},
				},
			},
			wantPass: false,
		},
		{
			// An empty needs-changes verdict is not "purely ordering" — there
			// is simply nothing to reason from. Mirrors allCrossPRBlocked's own
			// empty-slice rule.
			name:     "needs-changes with no findings is not elected",
			number:   10,
			verdict:  apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges},
			wantPass: false,
		},
		{
			name:   "an already-passing verdict is untouched",
			number: 10,
			verdict: apiv1.Verdict{
				Decision: apiv1.VerdictPass,
				Findings: []apiv1.Finding{blockedFinding(11)},
			},
			wantPass: false,
		},
		{
			// `fail` is a human judgment call and election must not launder it
			// into a merge.
			name:   "a fail verdict is never elected into a pass",
			number: 10,
			verdict: apiv1.Verdict{
				Decision: apiv1.VerdictFail,
				Findings: []apiv1.Finding{blockedFinding(11)},
			},
			wantPass: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elected, rationale := electedLanderPass(tt.number, tt.verdict)
			if elected != tt.wantPass {
				t.Fatalf("electedLanderPass(#%d, %s) = %v, want %v", tt.number, tt.verdict.Decision, elected, tt.wantPass)
			}
			if !elected {
				if rationale != "" {
					t.Errorf("not elected but got rationale %q, want empty", rationale)
				}
				return
			}
			// A derived pass must be self-explanatory. A reader seeing `pass`
			// on a PR whose findings all say "blocked" has to be able to tell
			// that a deterministic rule changed the decision, and which rule.
			for _, want := range []string{"Elected lander", "fifo"} {
				if !strings.Contains(rationale, want) {
					t.Errorf("rationale = %q, want it to mention %q", rationale, want)
				}
			}
			for _, b := range unionBlockingPRs(tt.verdict.Findings) {
				if !strings.Contains(rationale, "#"+strconv.Itoa(b)) {
					t.Errorf("rationale = %q, want it to name blocker #%d", rationale, b)
				}
			}
		})
	}
}

// TestElectedLanderPassAgreesWithElectLander pins the one real hazard in
// re-deriving the election inside apply-verdict rather than threading it: two
// stages could drift and disagree about who was crowned.
//
// They cannot be collapsed into one. `elected` is emitted only by elect-lander,
// and apply-verdict is ALSO reached straight from the review gate — where the
// preceding task is gather-sibling-context, which has no election to report —
// so a single-hop inputsFrom edge would fail closed on that path. Re-deriving
// is exact rather than approximate because electionDecision is a pure function
// of {selectedNumber, findings, policy}, all of which apply-verdict already
// holds. This test is what keeps that true.
func TestElectedLanderPassAgreesWithElectLander(t *testing.T) {
	policy, _ := resolveElectionPolicy(defaultElectionPolicy)

	cases := []struct {
		number   int
		findings []apiv1.Finding
	}{
		{10, []apiv1.Finding{blockedFinding(11, 12)}},
		{12, []apiv1.Finding{blockedFinding(10, 11)}},
		{10, []apiv1.Finding{blockedFinding(11), {Class: apiv1.FindingSubstantive, Message: "bug"}}},
		{10, nil},
		{10, []apiv1.Finding{blockedFinding(9)}},
	}

	for _, c := range cases {
		wantElected := electionDecision(c.findings, c.number, policy)
		gotElected, _ := electedLanderPass(c.number, apiv1.Verdict{
			Decision: apiv1.VerdictNeedsChanges,
			Findings: c.findings,
		})
		if gotElected != wantElected {
			t.Errorf("PR #%d findings=%d: apply-verdict derived elected=%v but elect-lander decides %v — "+
				"the two stages MUST agree or a PR is crowned by one and parked by the other",
				c.number, len(c.findings), gotElected, wantElected)
		}
	}
}
