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
		{"finding gaggle differs from partition", func() []apiv1.GaggleHealthEvent {
			candidate := event(1, now, apiv1.GaggleHealthFindingOpened, base)
			candidate.Gaggle = "beta"
			return []apiv1.GaggleHealthEvent{candidate}
		}()},
		{"episode key differs from identity", func() []apiv1.GaggleHealthEvent {
			candidate := event(1, now, apiv1.GaggleHealthFindingOpened, base)
			candidate.Finding.Identity.Run = "another-run"
			return []apiv1.GaggleHealthEvent{candidate}
		}()},
		{"opened with resolved state", func() []apiv1.GaggleHealthEvent {
			candidate := event(1, now, apiv1.GaggleHealthFindingOpened, base)
			candidate.Finding.ResolvedAt = &now
			candidate.Finding.ResolutionEvidence = candidate.Finding.Evidence
			candidate.Finding.Repair.FollowUp = apiv1.GaggleHealthFollowUpResolved
			return []apiv1.GaggleHealthEvent{candidate}
		}()},
		{"repair started with unchanged disposition", []apiv1.GaggleHealthEvent{
			event(1, now, apiv1.GaggleHealthFindingOpened, base),
			event(2, now, apiv1.GaggleHealthRepairStartedEvent, base),
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
		{"oversized identity", func(f *apiv1.GaggleHealthFinding) {
			f.Identity.Run = strings.Repeat("x", MaxIdentityLength+1)
			f.EpisodeKey, _ = EpisodeKey(f.Code, f.Identity)
		}},
		{"host path identity", func(f *apiv1.GaggleHealthFinding) {
			f.Identity.Run = `C:\Users\operator\run`
			f.EpisodeKey, _ = EpisodeKey(f.Code, f.Identity)
		}},
		{"malformed evidence digest", func(f *apiv1.GaggleHealthFinding) { f.Evidence[0].Digest = "abc" }},
		{"oversized evidence kind", func(f *apiv1.GaggleHealthFinding) {
			f.Evidence[0].Kind = strings.Repeat("x", MaxEvidenceKindLength+1)
		}},
		{"oversized idempotency key", func(f *apiv1.GaggleHealthFinding) {
			f.Repair.IdempotencyKey = strings.Repeat("x", MaxIdentityLength+1)
		}},
		{"secret shaped summary", func(f *apiv1.GaggleHealthFinding) {
			f.Summary = "token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := base
			candidate.Evidence = append([]apiv1.GaggleHealthEvidence(nil), base.Evidence...)
			candidate.ResolutionEvidence = append([]apiv1.GaggleHealthEvidence(nil), base.ResolutionEvidence...)
			tc.mutate(&candidate)
			if err := ValidateFinding(candidate); err == nil {
				t.Fatal("ValidateFinding() accepted invalid data")
			}
		})
	}
}
