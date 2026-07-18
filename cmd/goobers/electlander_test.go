package main

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestElectedLander is #833's election policy in isolation: lowest PR number
// wins its cluster — thisPR is the lander iff its number is below every PR it
// is blocked on.
func TestElectedLander(t *testing.T) {
	tests := []struct {
		name     string
		thisPR   int
		blockers []int
		want     bool
	}{
		{"lowest of the cluster wins", 810, []int{811, 812}, true},
		{"a higher member parks (a lower blocker exists)", 811, []int{810, 812}, false},
		{"highest member parks", 812, []int{810, 811}, false},
		{"no named blockers trivially wins", 810, nil, true},
		{"single higher blocker wins", 810, []int{999}, true},
		{"single lower blocker parks", 999, []int{810}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := electedLander(tt.thisPR, tt.blockers); got != tt.want {
				t.Fatalf("electedLander(%d, %v) = %v, want %v", tt.thisPR, tt.blockers, got, tt.want)
			}
		})
	}
}

// TestElectionDecision is the composite gate the elect-lander stage applies:
// election fires only for a verdict that is ENTIRELY cross-PR-ordering asks
// (the PR is individually fine, merely sibling-blocked) AND wins its cluster.
// Any real defect makes the PR non-electable regardless of numbering.
func TestElectionDecision(t *testing.T) {
	crossPR := func(blockers ...int) apiv1.Finding {
		return apiv1.Finding{Class: apiv1.FindingCrossPRBlocked, BlockingPRs: blockers}
	}
	substantive := apiv1.Finding{Class: apiv1.FindingSubstantive}

	tests := []struct {
		name     string
		findings []apiv1.Finding
		thisPR   int
		want     bool
	}{
		{"all cross-pr, lowest -> elected", []apiv1.Finding{crossPR(811)}, 810, true},
		{"all cross-pr, not lowest -> parked", []apiv1.Finding{crossPR(810)}, 811, false},
		{"cross-pr + a real defect -> not electable", []apiv1.Finding{crossPR(811), substantive}, 810, false},
		{"empty findings -> not electable", nil, 810, false},
		{"multiple cross-pr findings, lowest overall -> elected", []apiv1.Finding{crossPR(811), crossPR(812)}, 810, true},
		{"multiple cross-pr findings, a lower one present -> parked", []apiv1.Finding{crossPR(812), crossPR(809)}, 810, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := electionDecision(tt.findings, tt.thisPR); got != tt.want {
				t.Fatalf("electionDecision(%v, %d) = %v, want %v", tt.findings, tt.thisPR, got, tt.want)
			}
		})
	}
}
