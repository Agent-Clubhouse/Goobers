package gagglehealth

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestValidatePolicy(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*apiv1.GaggleHealthPolicy)
		wantErr string
	}{
		{name: "defaults"},
		{name: "unknown finding", mutate: func(p *apiv1.GaggleHealthPolicy) {
			p.Findings["invented"] = apiv1.GaggleFindingPolicy{Mode: "observe"}
		}, wantErr: "not a known finding"},
		{name: "unsafe repair", mutate: func(p *apiv1.GaggleHealthPolicy) {
			p.Findings[FindingNoProgress] = apiv1.GaggleFindingPolicy{Mode: "repair"}
		}, wantErr: "hard safety"},
		{name: "lower severity", mutate: func(p *apiv1.GaggleHealthPolicy) {
			p.Findings[FindingNoProgress] = apiv1.GaggleFindingPolicy{Mode: "observe", Severity: "warning"}
		}, wantErr: "cannot lower"},
		{name: "bad threshold", mutate: func(p *apiv1.GaggleHealthPolicy) {
			p.Thresholds.NoProgress = "1m"
		}, wantErr: "at least evaluationInterval"},
		{name: "enabled workflow requires target", mutate: func(p *apiv1.GaggleHealthPolicy) {
			p.EventWorkflow = &apiv1.GaggleHealthEventWorkflow{Enabled: true}
		}, wantErr: "workflow is required"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy := DefaultPolicy()
			if tc.mutate != nil {
				tc.mutate(&policy)
			}
			err := ValidatePolicy(policy)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("ValidatePolicy() error = %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("ValidatePolicy() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestAggregateStatePrecedence(t *testing.T) {
	tests := []struct {
		name     string
		states   []apiv1.GaggleHealthState
		expected apiv1.GaggleHealthState
	}{
		{"none", nil, apiv1.GaggleHealthHealthy},
		{"recovering", []apiv1.GaggleHealthState{apiv1.GaggleHealthRecovering}, apiv1.GaggleHealthRecovering},
		{"overlap", []apiv1.GaggleHealthState{apiv1.GaggleHealthDegraded, apiv1.GaggleHealthStalled}, apiv1.GaggleHealthStalled},
		{"inconsistent wins stalled", []apiv1.GaggleHealthState{apiv1.GaggleHealthStalled, apiv1.GaggleHealthInconsistent}, apiv1.GaggleHealthInconsistent},
		{"operator wins", []apiv1.GaggleHealthState{apiv1.GaggleHealthOperatorRequired, apiv1.GaggleHealthInconsistent}, apiv1.GaggleHealthOperatorRequired},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := make([]apiv1.GaggleHealthFinding, len(tc.states))
			for i, state := range tc.states {
				findings[i].Contribution = state
			}
			if got := AggregateState(findings); got != tc.expected {
				t.Fatalf("AggregateState() = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestEpisodeDedupeAndRestartReconstruction(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	identity := apiv1.GaggleHealthIdentity{Gaggle: "alpha", Workflow: "implementation", Run: "run-1"}
	key, err := EpisodeKey(FindingNoProgress, identity)
	if err != nil {
		t.Fatal(err)
	}

	first := finding(FindingNoProgress, key, identity, now, apiv1.GaggleHealthStalled)
	repeated := first
	repeated.LastObserved = now.Add(5 * time.Minute)
	repeated.Evidence = []apiv1.GaggleHealthEvidence{{Kind: "journal-event", Run: "run-1", Sequence: 9}}
	extended, err := ExtendEpisode(first, repeated)
	if err != nil {
		t.Fatal(err)
	}

	if extended.ObservationCount != 2 || !extended.FirstObserved.Equal(now) || len(extended.Evidence) != 2 {
		t.Fatalf("extended episode = %+v", extended)
	}

	degradedIdentity := apiv1.GaggleHealthIdentity{Gaggle: "alpha", Workflow: "curation"}
	degradedKey, err := EpisodeKey(FindingTriggerSilence, degradedIdentity)
	if err != nil {
		t.Fatal(err)
	}
	degraded := finding(FindingTriggerSilence, degradedKey, degradedIdentity, now, apiv1.GaggleHealthDegraded)
	resolved := extended
	resolvedAt := now.Add(10 * time.Minute)
	resolved.ResolvedAt = &resolvedAt
	resolved.ResolutionEvidence = []apiv1.GaggleHealthEvidence{{Kind: "journal-event", Run: "run-1", Sequence: 10}}
	resolved.Repair.FollowUp = apiv1.GaggleHealthFollowUpResolved
	events := []apiv1.GaggleHealthEvent{
		event(1, now, apiv1.GaggleHealthFindingOpened, first),
		event(2, now.Add(time.Minute), apiv1.GaggleHealthFindingOpened, degraded),
		event(3, repeated.LastObserved, apiv1.GaggleHealthFindingUpdated, extended),
		event(4, resolvedAt, apiv1.GaggleHealthFindingResolved, resolved),
	}
	got, err := Rebuild("alpha", events)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != apiv1.GaggleHealthDegraded || len(got.Active) != 1 || len(got.History) != 1 ||
		got.LastSequence != 4 || got.History[0].ObservationCount != 2 {
		t.Fatalf("rebuilt snapshot = %+v", got)
	}
	gotAgain, err := Rebuild("alpha", events)
	if err != nil {
		t.Fatal(err)
	}
	if gotAgain.State != got.State || gotAgain.LastSequence != got.LastSequence ||
		gotAgain.Active[0].EpisodeKey != got.Active[0].EpisodeKey {
		t.Fatalf("restart projection differs: first=%+v second=%+v", got, gotAgain)
	}
}

func finding(code, key string, identity apiv1.GaggleHealthIdentity, observed time.Time, state apiv1.GaggleHealthState) apiv1.GaggleHealthFinding {
	severity := findingRules[code].severity
	return apiv1.GaggleHealthFinding{
		SchemaVersion:      apiv1.GaggleHealthSchemaVersion,
		Code:               code,
		Severity:           severity,
		Contribution:       state,
		Identity:           identity,
		FirstObserved:      observed,
		LastObserved:       observed,
		ObservationCount:   1,
		EpisodeKey:         key,
		Evidence:           []apiv1.GaggleHealthEvidence{{Kind: "journal-event", Run: identity.Run, Sequence: 1}},
		Summary:            "bounded summary",
		Confidence:         0.9,
		EvidenceAssessment: "journal sequence proves no progress",
		Repair: apiv1.GaggleHealthRepair{
			RecommendedAction: "inspect the affected run",
			Disposition:       apiv1.GaggleHealthRepairNotAttempted,
			FollowUp:          apiv1.GaggleHealthFollowUpNone,
		},
	}
}

func event(sequence uint64, at time.Time, eventType apiv1.GaggleHealthEventType, finding apiv1.GaggleHealthFinding) apiv1.GaggleHealthEvent {
	copy := finding
	return apiv1.GaggleHealthEvent{
		SchemaVersion: apiv1.GaggleHealthSchemaVersion,
		Sequence:      sequence,
		OccurredAt:    at,
		Type:          eventType,
		Gaggle:        finding.Identity.Gaggle,
		EpisodeKey:    finding.EpisodeKey,
		Finding:       &copy,
	}
}
