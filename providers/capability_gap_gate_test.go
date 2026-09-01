package providers

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubGapLookup answers GetWorkItem from a fixed id -> state table, and
// records which ids were actually asked for.
type stubGapLookup struct {
	states map[string]string
	err    error
	asked  []string
}

func (s *stubGapLookup) GetWorkItem(_ context.Context, _ RepositoryRef, id string) (WorkItem, error) {
	s.asked = append(s.asked, id)
	if s.err != nil {
		return WorkItem{}, s.err
	}
	state, ok := s.states[id]
	if !ok {
		return WorkItem{}, errors.New("not found")
	}
	return WorkItem{ID: id, State: state}, nil
}

func gapGateRepo() RepositoryRef {
	return RepositoryRef{Provider: ProviderGitHub, Owner: "Agent-Clubhouse", Name: "Goobers"}
}

// TestTrackedGapReferencesExcludePermanentGaps is the registry-side half of
// #3058: only fixable gaps name an issue, so only they are the gate's
// business.
func TestTrackedGapReferencesExcludePermanentGaps(t *testing.T) {
	restore := swapKnownGaps(t, map[ProviderKind]map[Capability]CapabilityGap{
		ProviderADO: {
			CapBacklogBlockers: {Kind: GapTracked, Issue: "#3030"},
			CapPRQueryAssignee: {Kind: GapNotApplicable, Rationale: "no such concept"},
		},
	})
	defer restore()

	refs := TrackedGapReferences()
	if len(refs) != 1 {
		t.Fatalf("TrackedGapReferences() = %+v, want exactly the tracked entry", refs)
	}
	if refs[0].Provider != ProviderADO || refs[0].Capability != CapBacklogBlockers || refs[0].Issue != "#3030" {
		t.Errorf("TrackedGapReferences()[0] = %+v", refs[0])
	}
}

func TestValidateTrackedGapsOpenAcceptsOpenReferences(t *testing.T) {
	restore := swapKnownGaps(t, map[ProviderKind]map[Capability]CapabilityGap{
		ProviderADO: {
			CapBacklogBlockers: {Kind: GapTracked, Issue: "#3030"},
			CapPRQueryAssignee: {Kind: GapNotApplicable, Rationale: "no such concept"},
		},
	})
	defer restore()

	lookup := &stubGapLookup{states: map[string]string{"3030": "open"}}
	if errs := ValidateTrackedGapsOpen(context.Background(), lookup, gapGateRepo()); len(errs) != 0 {
		t.Fatalf("ValidateTrackedGapsOpen() = %v, want no errors", errs)
	}
	if len(lookup.asked) != 1 || lookup.asked[0] != "3030" {
		t.Errorf("looked up %v, want only the tracked issue (permanent gaps are never resolved)", lookup.asked)
	}
}

func TestValidateTrackedGapsOpenRejectsClosedReference(t *testing.T) {
	restore := swapKnownGaps(t, map[ProviderKind]map[Capability]CapabilityGap{
		ProviderADO: {CapPRQueryAssignee: {Kind: GapTracked, Issue: "#2178"}},
	})
	defer restore()

	lookup := &stubGapLookup{states: map[string]string{"2178": "closed"}}
	errs := ValidateTrackedGapsOpen(context.Background(), lookup, gapGateRepo())
	if len(errs) != 1 {
		t.Fatalf("ValidateTrackedGapsOpen() = %v, want one error", errs)
	}
	for _, want := range []string{"#2178", string(CapPRQueryAssignee), string(GapNotApplicable)} {
		if !strings.Contains(errs[0].Error(), want) {
			t.Errorf("error = %q, want it to mention %q", errs[0], want)
		}
	}
}

func TestValidateTrackedGapsOpenRejectsUnresolvableReferences(t *testing.T) {
	tests := []struct {
		name   string
		gap    CapabilityGap
		lookup *stubGapLookup
		want   string
	}{
		{
			name:   "lookup failure",
			gap:    CapabilityGap{Kind: GapTracked, Issue: "#3030"},
			lookup: &stubGapLookup{err: errors.New("forge unreachable")},
			want:   "could not be resolved",
		},
		{
			name:   "malformed reference",
			gap:    CapabilityGap{Kind: GapTracked, Issue: "3030"},
			lookup: &stubGapLookup{states: map[string]string{"3030": "open"}},
			want:   "not of the form",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restore := swapKnownGaps(t, map[ProviderKind]map[Capability]CapabilityGap{
				ProviderADO: {CapBacklogBlockers: tc.gap},
			})
			defer restore()

			errs := ValidateTrackedGapsOpen(context.Background(), tc.lookup, gapGateRepo())
			if len(errs) != 1 {
				t.Fatalf("ValidateTrackedGapsOpen() = %v, want one error", errs)
			}
			if !strings.Contains(errs[0].Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", errs[0], tc.want)
			}
		})
	}
}

func TestValidateTrackedGapsOpenWithoutLookup(t *testing.T) {
	errs := ValidateTrackedGapsOpen(context.Background(), nil, gapGateRepo())
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "Agent-Clubhouse/Goobers") {
		t.Fatalf("ValidateTrackedGapsOpen(nil) = %v, want one error naming the repository", errs)
	}
}

func TestIssueStateIsClosed(t *testing.T) {
	for _, state := range []string{"closed", "Closed", "Completed", "Resolved", "Removed"} {
		if !issueStateIsClosed(state) {
			t.Errorf("issueStateIsClosed(%q) = false, want true", state)
		}
	}
	for _, state := range []string{"", "open", "Active", "New", "in progress"} {
		if issueStateIsClosed(state) {
			t.Errorf("issueStateIsClosed(%q) = true, want false (unknown states must not manufacture failures)", state)
		}
	}
}
