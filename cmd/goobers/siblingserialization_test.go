package main

import (
	"reflect"
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
