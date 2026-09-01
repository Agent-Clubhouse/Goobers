package main

import (
	"fmt"
	"reflect"
	"strconv"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestResolveSiblingSerialization(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      string
		wantKnown bool
	}{
		{"election is explicit", serializationElection, serializationElection, true},
		{"ordering is explicit", serializationOrdering, serializationOrdering, true},
		{"empty falls back to the default", "", defaultSiblingSerialization, false},
		{"unknown falls back to the default", "bogus", defaultSiblingSerialization, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, known := resolveSiblingSerialization(tt.input)
			if got != tt.want || known != tt.wantKnown {
				t.Fatalf("resolveSiblingSerialization(%q) = (%q, %v), want (%q, %v)", tt.input, got, known, tt.want, tt.wantKnown)
			}
		})
	}
	if defaultSiblingSerialization != serializationElection {
		t.Fatalf("default strategy must stay %q so shipped workflows are unchanged, got %q",
			serializationElection, defaultSiblingSerialization)
	}
}

func TestSerializationCluster(t *testing.T) {
	findings := []apiv1.Finding{blockedFinding(11, 9), blockedFinding(9, 10)}
	tests := []struct {
		name     string
		strategy string
		findings []apiv1.Finding
		overlaps []int
		selected int
		want     []int
	}{
		{
			name:     "election uses only the deterministic overlap set",
			strategy: serializationElection,
			findings: findings,
			overlaps: []int{12},
			selected: 10,
			want:     []int{12},
		},
		{
			name:     "election without an overlap set has no cluster",
			strategy: serializationElection,
			findings: findings,
			selected: 10,
			want:     nil,
		},
		{
			name:     "ordering serializes on reviewer-named blockers alone",
			strategy: serializationOrdering,
			findings: findings,
			selected: 10,
			want:     []int{9, 11},
		},
		{
			name:     "ordering unions blockers with the overlap set, deduped and sorted",
			strategy: serializationOrdering,
			findings: findings,
			overlaps: []int{12, 9},
			selected: 10,
			want:     []int{9, 11, 12},
		},
		{
			name:     "the selected PR is never in its own cluster",
			strategy: serializationOrdering,
			findings: []apiv1.Finding{blockedFinding(10, 11)},
			overlaps: []int{10},
			selected: 10,
			want:     []int{11},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serializationCluster(tt.strategy, tt.findings, tt.overlaps, tt.selected)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("serializationCluster(%q) = %v, want %v", tt.strategy, got, tt.want)
			}
		})
	}
}

// A gaggle that omits the election machinery has no deterministic overlap set,
// so under the default strategy nothing serializes its cluster and every member
// sits at needs-changes forever (#2741). Under "ordering" the same reviewer
// findings resolve to exactly one lander, with the rest parked behind it.
func TestSerializationOrderingBreaksTheNoOverlapSetDeadlock(t *testing.T) {
	findings := []apiv1.Finding{blockedFinding(11, 12)}
	policy, policyName := resolveElectionPolicy(defaultElectionPolicy)

	electionCluster := serializationCluster(serializationElection, findings, nil, 10)
	if _, rationale := resolveElectionOutcome(10, apiv1.VerdictNeedsChanges, findings, "", electionCluster, nil, policy, policyName); rationale != "" {
		t.Fatalf("election strategy must stay a no-op without an overlap set, got %q", rationale)
	}

	orderingCluster := serializationCluster(serializationOrdering, findings, nil, 10)
	elected, rationale := resolveElectionOutcome(10, apiv1.VerdictNeedsChanges, findings, "", orderingCluster, nil, policy, policyName)
	if !elected || rationale == "" {
		t.Fatalf("ordering strategy must elect the fifo winner #10, got elected=%v rationale=%q", elected, rationale)
	}

	loserFindings := []apiv1.Finding{blockedFinding(10, 12)}
	loserCluster := serializationCluster(serializationOrdering, loserFindings, nil, 11)
	if elected, _ := resolveElectionOutcome(11, apiv1.VerdictNeedsChanges, loserFindings, "", loserCluster, nil, policy, policyName); elected {
		t.Fatal("only one cluster member may be elected under fifo; #11 must defer to #10")
	}
}

// The published OverlapCluster/Elected pair MUST describe the cluster the
// configured strategy actually serialized over. Under "ordering" with no
// deterministic overlap set — the exact configuration #2741 exists to serve —
// an Elected:true published with OverlapCluster:false would falsify the
// normative invariant on the contract field that merge-pr's #1071 conjunct
// reads, leaving a crowned lander's evidence indistinguishable from a PR with
// no cluster at all.
func TestApplyVerdictOrderingPublishesClusterEvidenceWithoutOverlapSet(t *testing.T) {
	for _, tc := range []struct {
		name           string
		selectedNumber int
		findings       []apiv1.Finding
		wantDecision   apiv1.VerdictDecision
		wantElected    bool
	}{
		{
			name:           "fifo winner is crowned with cluster evidence",
			selectedNumber: 30,
			findings:       []apiv1.Finding{blockedFinding(31)},
			wantDecision:   apiv1.VerdictPass,
			wantElected:    true,
		},
		{
			name:           "loser is parked with cluster evidence and no election",
			selectedNumber: 31,
			findings:       []apiv1.Finding{blockedFinding(30)},
			wantDecision:   apiv1.VerdictNeedsChanges,
			wantElected:    false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			for _, number := range []int{30, 31} {
				server.addIssue(number, "Cluster PR")
				server.addOpenPR(number, fmt.Sprintf("goobers/implementation/run-%d", number), "main",
					fmt.Sprintf("sha%dhead", number), "shamainbase", false, nil, []fakePRFile{
						{path: fmt.Sprintf("internal/pkg%d/main.go", number), status: "modified", additions: 2, deletions: 1},
					})
			}

			const runID = "run-2741"
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", runID)
			t.Setenv("GOOBERS_WORKFLOW", "merge-review")
			t.Setenv("GOOBERS_GAGGLE", "goobers")
			t.Setenv("GOOBERS_CRED_GITHUB_PR_REVIEW", "review-token")
			t.Setenv("GOOBERS_INPUT_SELECTEDNUMBER", strconv.Itoa(tc.selectedNumber))
			t.Setenv("GOOBERS_INPUT_OVERLAPPINGSIBLINGS", "")
			t.Setenv("GOOBERS_INPUT_SIBLINGSERIALIZATION", serializationOrdering)

			seedGateVerdictJournal(t, root, runID, apiv1.Verdict{
				Decision: apiv1.VerdictNeedsChanges,
				Summary:  "waiting on a sibling",
				Findings: tc.findings,
				HeadSHA:  fmt.Sprintf("sha%dhead", tc.selectedNumber),
				BaseSHA:  "shamainbase",
			})

			t.Chdir(t.TempDir())
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
			if posted.Elected != tc.wantElected {
				t.Fatalf("posted.Elected = %v, want %v", posted.Elected, tc.wantElected)
			}
			if !posted.OverlapCluster {
				t.Fatalf("posted.OverlapCluster = false with Elected = %v, want true: the published pair must "+
					"describe the serialized cluster or merge-pr's #1071 conjunct never fires under %q",
					posted.Elected, serializationOrdering)
			}
		})
	}
}
