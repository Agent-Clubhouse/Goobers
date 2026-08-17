package main

import (
	"fmt"
	"reflect"
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

// TestResolveElectionOutcomeNeedsChanges pins the reframed meaning of
// election: being crowned means "those siblings no longer block you", NOT
// "merge regardless of review".
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
func TestResolveElectionOutcomeNeedsChanges(t *testing.T) {
	tests := []struct {
		name     string
		number   int
		decision apiv1.VerdictDecision
		findings []apiv1.Finding
		overlaps []int
		wantPass bool
	}{
		{
			// fifo: lowest number in the cluster goes first.
			name:     "lowest in cluster with only ordering findings is elected",
			number:   10,
			decision: apiv1.VerdictNeedsChanges,
			findings: []apiv1.Finding{blockedFinding(11, 12)},
			overlaps: []int{11, 12},
			wantPass: true,
		},
		{
			name:     "not lowest is not elected",
			number:   12,
			decision: apiv1.VerdictNeedsChanges,
			findings: []apiv1.Finding{blockedFinding(10, 11)},
			overlaps: []int{10, 11},
			wantPass: false,
		},
		{
			// The load-bearing safety property. A real defect alongside the
			// ordering asks must never be resolved away by election — the PR
			// genuinely needs work, and going first does not fix it.
			name:     "a real defect alongside ordering findings is never elected",
			number:   10,
			decision: apiv1.VerdictNeedsChanges,
			findings: []apiv1.Finding{
				blockedFinding(11),
				{Severity: apiv1.SeverityError, Message: "nil deref", Class: apiv1.FindingSubstantive},
			},
			overlaps: []int{11},
			wantPass: false,
		},
		{
			// An empty needs-changes verdict is not "purely ordering" — there
			// is simply nothing to reason from. Mirrors allCrossPRBlocked's own
			// empty-slice rule.
			name:     "needs-changes with no findings is not elected",
			number:   10,
			decision: apiv1.VerdictNeedsChanges,
			overlaps: []int{11},
			wantPass: false,
		},
		{
			// `fail` is a human judgment call and election must not launder it
			// into a merge.
			name:     "a fail verdict is never elected into a pass",
			number:   10,
			decision: apiv1.VerdictFail,
			findings: []apiv1.Finding{blockedFinding(11)},
			overlaps: []int{11},
			wantPass: false,
		},
		{
			// No deterministic overlap at all: the non-overlapping-PR path
			// must stay completely untouched (#1071 acceptance criterion).
			name:     "no overlapping siblings leaves the verdict untouched",
			number:   10,
			decision: apiv1.VerdictNeedsChanges,
			findings: []apiv1.Finding{blockedFinding(11)},
			overlaps: nil,
			wantPass: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elected, rationale := resolveElectionOutcome(tt.number, tt.decision, tt.findings, "", tt.overlaps, nil, electedLander, "fifo")
			if elected != tt.wantPass {
				t.Fatalf("resolveElectionOutcome(#%d, %s) = %v, want %v", tt.number, tt.decision, elected, tt.wantPass)
			}
			if !elected {
				if rationale != "" && len(tt.overlaps) > 0 && tt.decision == apiv1.VerdictPass {
					// A not-elected genuine pass gets an explanatory rationale
					// (covered by the genuine-pass test below); every case
					// here is needs-changes/fail, which stays silent.
					t.Errorf("rationale = %q, want empty for a needs-changes/fail case", rationale)
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
			for _, b := range unionBlockingPRs(tt.findings) {
				if !strings.Contains(rationale, "#"+strconv.Itoa(b)) {
					t.Errorf("rationale = %q, want it to name blocker #%d", rationale, b)
				}
			}
		})
	}
}

// TestResolveElectionOutcomeGenuinePass pins the #1071 fix: a reviewer's
// verdict that is ALREADY a genuine pass (no defect of its own) still must not
// reach merge-pr as a landing authority while it shares a deterministic file
// overlap with a live predecessor sibling — otherwise two overlapping PRs
// could each independently earn a clean pass and race GitHub's native merge
// queue with no arbitration between them at all.
func TestResolveElectionOutcomeGenuinePass(t *testing.T) {
	t.Run("crowned genuine pass is stamped elected and keeps passing", func(t *testing.T) {
		elected, rationale := resolveElectionOutcome(10, apiv1.VerdictPass, []apiv1.Finding{blockedFinding(11)}, "", []int{11}, nil, electedLander, "fifo")
		if !elected {
			t.Fatalf("resolveElectionOutcome = elected=false, want true (PR #10 has no predecessor)")
		}
		if !strings.Contains(rationale, "Elected lander") {
			t.Errorf("rationale = %q, want it to explain the crown", rationale)
		}
	})

	t.Run("non-crowned genuine pass must wait its turn", func(t *testing.T) {
		elected, rationale := resolveElectionOutcome(12, apiv1.VerdictPass, []apiv1.Finding{blockedFinding(10, 11)}, "", []int{10, 11}, nil, electedLander, "fifo")
		if elected {
			t.Fatalf("resolveElectionOutcome = elected=true, want false (PR #12 has live predecessors #10/#11)")
		}
		if rationale == "" {
			t.Fatal("want a non-empty rationale explaining why an individually-clean PR is parked")
		}
		for _, want := range []string{"#10", "#11", "fifo"} {
			if !strings.Contains(rationale, want) {
				t.Errorf("rationale = %q, want it to mention %q", rationale, want)
			}
		}
	})

	t.Run("a genuine pass with no overlap is never touched", func(t *testing.T) {
		elected, rationale := resolveElectionOutcome(10, apiv1.VerdictPass, nil, "", nil, nil, electedLander, "fifo")
		if elected || rationale != "" {
			t.Fatalf("resolveElectionOutcome(no overlap) = (%v, %q), want (false, \"\")", elected, rationale)
		}
	})
}

// TestResolveElectionOutcomeRacePolicy is #2268's fast-track lane at the
// resolveElectionOutcome level: a critical-lane PR with no defect of its own
// must land without waiting on an unrelated, live sibling's own blocker —
// including the reported case of a sibling stuck goobers:needs-human, which
// can block a lane indefinitely under fifo (PR #2266 blocked behind #2265 in
// the issue's evidence). "race" elects unconditionally the moment thisPR's
// own review clears; it never depends on which siblings are blocked or why.
func TestResolveElectionOutcomeRacePolicy(t *testing.T) {
	t.Run("critical-vs-routine: elects immediately despite lower-numbered live siblings", func(t *testing.T) {
		// Under fifo this exact input parks (see the mirror case in
		// TestResolveElectionOutcomeGenuinePass: PR #12 with predecessors
		// #10/#11 is NOT elected). race elects it anyway.
		elected, rationale := resolveElectionOutcome(12, apiv1.VerdictPass, []apiv1.Finding{blockedFinding(10, 11)}, "", []int{10, 11}, nil, electedRace, "race")
		if !elected {
			t.Fatalf("resolveElectionOutcome(race) = elected=false, want true — race must not wait on siblings' turn")
		}
		if !strings.Contains(rationale, "race") {
			t.Errorf("rationale = %q, want it to name the race policy", rationale)
		}
	})

	t.Run("critical-vs-blocked-routine: elects despite a needs-human-stuck sibling that would block fifo forever", func(t *testing.T) {
		// A sibling parked goobers:needs-human never self-resolves — under fifo
		// a higher-numbered critical PR blocked on it would wait indefinitely
		// (the exact #2268 failure mode). The sibling's own stuck state is not
		// even consulted here: election is a pure function of {thisPR,
		// blockers}, and race ignores blockers entirely.
		const needsHumanStuckSibling = 2265
		const criticalPR = 2266
		elected, _ := resolveElectionOutcome(criticalPR, apiv1.VerdictPass, []apiv1.Finding{blockedFinding(needsHumanStuckSibling)}, "", []int{needsHumanStuckSibling}, nil, electedRace, "race")
		if !elected {
			t.Fatalf("resolveElectionOutcome(race) = elected=false, want true — a needs-human sibling must not indefinitely block the critical lane")
		}
	})

	t.Run("a real defect on thisPR's own review is never elected, race included", func(t *testing.T) {
		elected, rationale := resolveElectionOutcome(10, apiv1.VerdictNeedsChanges, []apiv1.Finding{
			blockedFinding(11),
			{Severity: apiv1.SeverityError, Message: "nil deref", Class: apiv1.FindingSubstantive},
		}, "", []int{11}, nil, electedRace, "race")
		if elected {
			t.Fatal("race policy elected a PR carrying its own real defect — #1071 safety invariant violated")
		}
		if rationale != "" {
			t.Errorf("rationale = %q, want empty for a needs-changes case with a real defect", rationale)
		}
	})
}

// TestResolveElectionOutcomeAgreesWithElectLander pins the one real hazard in
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
func TestResolveElectionOutcomeAgreesWithElectLander(t *testing.T) {
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
		wantElected := electionDecision(c.findings, c.number, policy, nil)
		gotElected, _ := resolveElectionOutcome(c.number, apiv1.VerdictNeedsChanges, c.findings, "", []int{1}, nil, policy, defaultElectionPolicy)
		if gotElected != wantElected {
			t.Errorf("PR #%d findings=%d: apply-verdict derived elected=%v but elect-lander decides %v — "+
				"the two stages MUST agree or a PR is crowned by one and parked by the other",
				c.number, len(c.findings), gotElected, wantElected)
		}
	}
}

// TestApplyVerdictGenuinePassOverlapEndToEnd is #1071's end-to-end
// reproduction of the observed bypass: a reviewer's verdict that is ALREADY a
// clean pass (no defect of its own) still deterministically overlaps a live
// sibling. Before this fix, apply-verdict published that pass with no
// election evidence at all, so nothing prevented GitHub's native merge queue
// from landing it independently of any single-lander arbitration. Now the
// published verdict must carry OverlapCluster/Elected evidence either way:
// the crowned member keeps its pass with Elected stamped true, and the
// non-crowned member is parked blocked-on-sibling instead.
func TestApplyVerdictGenuinePassOverlapEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name           string
		selectedNumber int
		overlap        string
		wantDecision   apiv1.VerdictDecision
		wantLabel      string
		wantElected    bool
	}{
		{
			// A pass verdict never gets a label update (verdictLabel's
			// "goobers:merge-ready" is only ever set implicitly by the absence
			// of any other label) — wantLabel empty skips that assertion.
			name:           "crowned member keeps passing with elected evidence",
			selectedNumber: 20,
			overlap:        "21",
			wantDecision:   apiv1.VerdictPass,
			wantElected:    true,
		},
		{
			name:           "non-crowned member is parked blocked-on-sibling instead of landing ungated",
			selectedNumber: 22,
			overlap:        "21",
			wantDecision:   apiv1.VerdictNeedsChanges,
			wantLabel:      blockedOnSiblingLabel,
			wantElected:    false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(tc.selectedNumber, "Selected PR")
			server.addOpenPR(tc.selectedNumber, "goobers/implementation/run-x", "main", "shaselhead", "shamainbase", false, nil, nil)
			server.addIssue(21, "Sibling PR")
			server.addOpenPR(21, "goobers/implementation/run-21", "main", "sha21head", "shamainbase", false, nil, nil)

			const runID = "run-1071"
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
			t.Setenv("GOOBERS_WORKFLOW", "merge-review")
			t.Setenv("GOOBERS_GAGGLE", "goobers")
			t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
			t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", strconv.Itoa(tc.selectedNumber))
			t.Setenv("GOOBERS_INPUT_OVERLAPPINGSIBLINGS", tc.overlap)

			seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
				Decision: apiv1.VerdictPass, Summary: "clean", HeadSHA: "shaselhead", BaseSHA: "shamainbase",
			})

			applyDir := t.TempDir()
			t.Chdir(applyDir)
			if code, _, stderr := runArgs(t, "apply-verdict", root); code != 0 {
				t.Fatalf("apply-verdict: code = %d, stderr = %q", code, stderr)
			}

			server.mu.Lock()
			issue := server.issues[tc.selectedNumber]
			server.mu.Unlock()
			if len(issue.comments) == 0 {
				t.Fatalf("no verdict comment posted")
			}
			posted, ok := parseVerdictComment(issue.comments[len(issue.comments)-1])
			if !ok {
				t.Fatalf("posted comment has no recoverable verdict payload: %q", issue.comments[len(issue.comments)-1])
			}
			if posted.Decision != tc.wantDecision {
				t.Fatalf("posted.Decision = %q, want %q", posted.Decision, tc.wantDecision)
			}
			if !posted.OverlapCluster {
				t.Fatalf("posted.OverlapCluster = false, want true")
			}
			if posted.Elected != tc.wantElected {
				t.Fatalf("posted.Elected = %v, want %v", posted.Elected, tc.wantElected)
			}
			if tc.wantLabel != "" && !hasAllLabels(issue.labels, []string{tc.wantLabel}) {
				t.Fatalf("labels = %v, want %q", issue.labels, tc.wantLabel)
			}
		})
	}
}

func TestApplyVerdictPR2478OverlapOnlyRoutesToSiblingBlock(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	const selectedNumber = 2478
	server.addIssue(selectedNumber, "Selected PR", needsRemediationLabel)
	server.addOpenPR(selectedNumber, "goobers/implementation/run-x", "main", "selected-head", "main-base", false, []string{needsRemediationLabel}, []fakePRFile{
		{path: "docs/guides/quickstart.md", status: "modified", additions: 2, deletions: 1},
	})
	for _, number := range []int{2475, 2476, 2481} {
		server.addIssue(number, "Sibling PR")
		server.addOpenPR(number, fmt.Sprintf("goobers/implementation/run-%d", number), "main", fmt.Sprintf("head-%d", number), "main-base", false, nil, nil)
	}

	const runID = "run-2486"
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
	t.Setenv("GOOBERS_WORKFLOW", "merge-review")
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
	t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", strconv.Itoa(selectedNumber))
	t.Setenv("GOOBERS_INPUT_OVERLAPPINGSIBLINGS", "2481,2476,2475")
	seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Summary:  "Blocked by documentation overlap with PRs #2481, #2476, and #2475.",
		HeadSHA:  "selected-head",
		BaseSHA:  "main-base",
		Findings: []apiv1.Finding{
			{Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive, Message: "reconcile or sequence", Location: "PR #2481 (docs/guides/quickstart.md)"},
			{Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive, Message: "reconcile or sequence", Location: "PR #2476 (docs/guides/quickstart.md)"},
			{Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive, Message: "reconcile or establish merge ordering", Location: "PR #2475 (docs/guides/quickstart.md, docs/guides/quickstart-linux.md)"},
		},
	})

	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "apply-verdict", root); code != 0 {
		t.Fatalf("apply-verdict: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	server.mu.Lock()
	issue := server.issues[selectedNumber]
	server.mu.Unlock()
	if !hasAllLabels(issue.labels, []string{blockedOnSiblingLabel}) {
		t.Fatalf("labels = %v, want %q", issue.labels, blockedOnSiblingLabel)
	}
	if hasAllLabels(issue.labels, []string{needsRemediationLabel}) {
		t.Fatalf("labels = %v, overlap-only verdict retained stale remediation label", issue.labels)
	}
	posted, ok := parseVerdictComment(issue.comments[len(issue.comments)-1])
	if !ok {
		t.Fatalf("posted comment has no recoverable verdict payload: %q", issue.comments[len(issue.comments)-1])
	}
	if !allCrossPRBlocked(posted.Findings) {
		t.Fatalf("posted findings = %+v, want cross-pr-blocked representation", posted.Findings)
	}
	if want := []int{2475, 2476, 2481}; !reflect.DeepEqual(unionBlockingPRs(posted.Findings), want) {
		t.Fatalf("posted blockers = %v, want %v", unionBlockingPRs(posted.Findings), want)
	}
}

// TestApplyVerdictElectsOverNeedsHumanSibling verifies that a PR which requires
// human intervention is not allowed to hold the lander crown under any policy.
func TestApplyVerdictElectsOverNeedsHumanSibling(t *testing.T) {
	const stuckSibling = 2265 // lower number, would win fifo — carries goobers:needs-human
	const criticalPR = 2266

	for _, tc := range []struct {
		name         string
		policy       string
		wantDecision apiv1.VerdictDecision
		wantLabel    string
		wantElected  bool
	}{
		{
			name:         "fifo: skips the needs-human sibling",
			policy:       "fifo",
			wantDecision: apiv1.VerdictPass,
			wantElected:  true,
		},
		{
			name:         "race: elected and lands despite the needs-human sibling",
			policy:       "race",
			wantDecision: apiv1.VerdictPass,
			wantElected:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(criticalPR, "Critical PR")
			server.addOpenPR(criticalPR, "goobers/implementation-critical/run-x", "main", "shacriticalhead", "shamainbase", false, nil, nil)
			server.addIssue(stuckSibling, "Stuck sibling", "goobers:needs-human")
			server.addOpenPR(stuckSibling, "goobers/implementation/run-2265", "main", "sha2265head", "shamainbase", false, []string{"goobers:needs-human"}, nil)

			runID := "run-2268-" + tc.policy
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
			t.Setenv("GOOBERS_WORKFLOW", "merge-review-critical")
			t.Setenv("GOOBERS_GAGGLE", "goobers")
			t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
			t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", strconv.Itoa(criticalPR))
			t.Setenv("GOOBERS_INPUT_OVERLAPPINGSIBLINGS", strconv.Itoa(stuckSibling))
			t.Setenv("GOOBERS_INPUT_ELECTIONPOLICY", tc.policy)

			seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
				Decision: apiv1.VerdictPass, Summary: "clean, no defect of its own", HeadSHA: "shacriticalhead", BaseSHA: "shamainbase",
			})

			applyDir := t.TempDir()
			t.Chdir(applyDir)
			if code, _, stderr := runArgs(t, "apply-verdict", root); code != 0 {
				t.Fatalf("apply-verdict: code = %d, stderr = %q", code, stderr)
			}

			server.mu.Lock()
			issue := server.issues[criticalPR]
			server.mu.Unlock()
			if len(issue.comments) == 0 {
				t.Fatalf("no verdict comment posted")
			}
			posted, ok := parseVerdictComment(issue.comments[len(issue.comments)-1])
			if !ok {
				t.Fatalf("posted comment has no recoverable verdict payload: %q", issue.comments[len(issue.comments)-1])
			}
			if posted.Decision != tc.wantDecision {
				t.Fatalf("posted.Decision = %q, want %q", posted.Decision, tc.wantDecision)
			}
			if posted.Elected != tc.wantElected {
				t.Fatalf("posted.Elected = %v, want %v", posted.Elected, tc.wantElected)
			}
			if tc.wantLabel != "" && !hasAllLabels(issue.labels, []string{tc.wantLabel}) {
				t.Fatalf("labels = %v, want %q", issue.labels, tc.wantLabel)
			}
		})
	}
}
