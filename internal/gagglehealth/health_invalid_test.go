package gagglehealth

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestResolvePolicyPreservesConservativeDefaults(t *testing.T) {
	resolved, err := ResolvePolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Enabled == nil || !*resolved.Enabled ||
		resolved.Findings[FindingOrphanedClaim].Mode != string(apiv1.GaggleHealthRepairMode) {
		t.Fatalf("omitted policy resolved to %+v", resolved)
	}

	disabled := false
	resolved, err = ResolvePolicy(&apiv1.GaggleHealthPolicy{
		Enabled: &disabled,
		Thresholds: &apiv1.GaggleHealthThresholds{
			NoProgress: "45m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Enabled == nil || *resolved.Enabled ||
		resolved.Thresholds.NoProgress != "45m" ||
		resolved.Thresholds.TriggerSilence != "30m" ||
		resolved.Notifications.MinimumSeverity != string(apiv1.GaggleHealthSeverityWarning) {
		t.Fatalf("partial policy resolved to %+v", resolved)
	}

	notificationsEnabled := false
	resolved, err = ResolvePolicy(&apiv1.GaggleHealthPolicy{
		EvaluationInterval: "10m",
		Thresholds: &apiv1.GaggleHealthThresholds{
			TriggerSilence:       "40m",
			NoProgress:           "50m",
			FlappingWindow:       "2h",
			FlappingCount:        5,
			ProlongedDegradation: "8h",
			EvidenceRetention:    "800h",
		},
		Findings: map[string]apiv1.GaggleFindingPolicy{
			FindingTriggerSilence: {Mode: string(apiv1.GaggleHealthEscalate), Severity: string(apiv1.GaggleHealthSeverityError)},
		},
		Notifications: &apiv1.GaggleHealthNotifications{
			Enabled:         &notificationsEnabled,
			EscalateAfter:   "2h",
			MinimumSeverity: string(apiv1.GaggleHealthSeverityError),
		},
		EventWorkflow: &apiv1.GaggleHealthEventWorkflow{
			Enabled: true, Workflow: "health-events",
			EventTypes:   []string{string(apiv1.GaggleHealthFindingOpened)},
			FindingCodes: []string{FindingTriggerSilence},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.EvaluationInterval != "10m" || resolved.Thresholds.FlappingCount != 5 ||
		resolved.Notifications.Enabled == nil || *resolved.Notifications.Enabled ||
		resolved.EventWorkflow.Workflow != "health-events" {
		t.Fatalf("complete policy resolved to %+v", resolved)
	}

	if _, err := ResolvePolicy(&apiv1.GaggleHealthPolicy{
		Findings: map[string]apiv1.GaggleFindingPolicy{"invented": {Mode: "observe"}},
	}); err == nil {
		t.Fatal("ResolvePolicy() accepted an unknown finding")
	}
}

func TestRebuildRejectsInvalidJournalTransitions(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	identity := apiv1.GaggleHealthIdentity{Gaggle: "alpha", Run: "run-1"}
	key, err := EpisodeKey(FindingNoProgress, identity)
	if err != nil {
		t.Fatal(err)
	}
	base := finding(FindingNoProgress, key, identity, now, apiv1.GaggleHealthStalled)
	tests := []struct {
		name   string
		events []apiv1.GaggleHealthEvent
	}{
		{"wrong gaggle", []apiv1.GaggleHealthEvent{event(1, now, apiv1.GaggleHealthFindingOpened, base), {
			SchemaVersion: apiv1.GaggleHealthSchemaVersion, Sequence: 2, OccurredAt: now, Type: apiv1.GaggleHealthEvaluated, Gaggle: "beta",
		}}},
		{"duplicate sequence", []apiv1.GaggleHealthEvent{
			event(1, now, apiv1.GaggleHealthFindingOpened, base),
			event(1, now, apiv1.GaggleHealthFindingUpdated, base),
		}},
		{"update before open", []apiv1.GaggleHealthEvent{event(1, now, apiv1.GaggleHealthFindingUpdated, base)}},
		{"resolve without time", []apiv1.GaggleHealthEvent{
			event(1, now, apiv1.GaggleHealthFindingOpened, base),
			event(2, now, apiv1.GaggleHealthFindingResolved, base),
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Rebuild("alpha", tc.events); err == nil {
				t.Fatal("Rebuild() accepted an invalid journal")
			}
		})
	}
}

func TestValidateFindingRejectsUnboundedOrUnauthorizedData(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	identity := apiv1.GaggleHealthIdentity{Gaggle: "alpha"}
	key, err := EpisodeKey(FindingNoProgress, identity)
	if err != nil {
		t.Fatal(err)
	}
	base := finding(FindingNoProgress, key, identity, now, apiv1.GaggleHealthStalled)
	tests := []struct {
		name   string
		mutate func(*apiv1.GaggleHealthFinding)
	}{
		{"low severity", func(f *apiv1.GaggleHealthFinding) { f.Severity = apiv1.GaggleHealthSeverityWarning }},
		{"unbounded summary", func(f *apiv1.GaggleHealthFinding) { f.Summary = strings.Repeat("x", MaxSummaryLength+1) }},
		{"unauthorized repair", func(f *apiv1.GaggleHealthFinding) { f.Repair.PolicyAuthorized = true }},
		{"invalid confidence", func(f *apiv1.GaggleHealthFinding) { f.Confidence = 2 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := base
			tc.mutate(&candidate)
			if err := ValidateFinding(candidate); err == nil {
				t.Fatal("ValidateFinding() accepted invalid data")
			}
		})
	}
}
