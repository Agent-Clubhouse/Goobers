package readservice

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestProjectEventClassifiesEveryKnownEventType(t *testing.T) {
	tests := []struct {
		event    journal.Event
		category RunEventCategory
		chapter  bool
	}{
		{event: journal.Event{Type: journal.EventRunStarted}, category: RunEventTransition, chapter: true},
		{event: journal.Event{Type: journal.EventRunResumed}, category: RunEventTransition, chapter: true},
		{event: journal.Event{Type: journal.EventRunFinished}, category: RunEventTransition, chapter: true},
		{event: journal.Event{Type: journal.EventStageStarted}, category: RunEventTransition, chapter: true},
		{event: journal.Event{Type: journal.EventStageHeartbeat}, category: RunEventLiveness},
		{event: journal.Event{Type: journal.EventStageFinished}, category: RunEventTransition, chapter: true},
		{event: journal.Event{Type: journal.EventStageRerunRequested}, category: RunEventTransition, chapter: true},
		{event: journal.Event{Type: journal.EventGateStarted}, category: RunEventBookkeeping},
		{event: journal.Event{Type: journal.EventGatePaused}, category: RunEventTransition, chapter: true},
		{event: journal.Event{Type: journal.EventGateEvaluated}, category: RunEventDecision, chapter: true},
		{event: journal.Event{Type: journal.EventArtifactRecorded}, category: RunEventEvidence},
		{event: journal.Event{Type: journal.EventSpanRecorded}, category: RunEventEvidence},
		{event: journal.Event{Type: journal.EventInputSnapshot}, category: RunEventEvidence},
		{
			event: journal.Event{
				Type:        journal.EventRefTouched,
				ExternalRef: &journal.ExternalRef{Kind: "pr"},
			},
			category: RunEventResult,
			chapter:  true,
		},
		{event: journal.Event{Type: journal.EventError}, category: RunEventResult, chapter: true},
		{event: journal.Event{Type: journal.EventRedaction}, category: RunEventBookkeeping},
		{event: journal.Event{Type: journal.EventRepaired}, category: RunEventBookkeeping},
		{event: journal.Event{Type: journal.EventRunnerAnnotation}, category: RunEventBookkeeping},
		{event: journal.Event{Type: journal.EventTriggerFired}, category: RunEventTransition, chapter: true},
		{event: journal.Event{Type: journal.EventTickSkipped}, category: RunEventDecision, chapter: true},
		{event: journal.Event{Type: journal.EventWorkflowStarved}, category: RunEventResult, chapter: true},
		{event: journal.Event{Type: journal.EventProviderQuotaReset}, category: RunEventLiveness},
		{event: journal.Event{Type: journal.EventPollShed}, category: RunEventLiveness},
		{event: journal.Event{Type: journal.EventClaimAcquired}, category: RunEventBookkeeping},
		{event: journal.Event{Type: journal.EventClaimReleased}, category: RunEventBookkeeping},
		{event: journal.Event{Type: journal.EventClaimForceReleased}, category: RunEventBookkeeping},
		{event: journal.Event{Type: journal.EventClaimLockSlow}, category: RunEventBookkeeping},
		{event: journal.Event{Type: journal.EventClaimLockTimeout}, category: RunEventResult, chapter: true},
		{event: journal.Event{Type: journal.EventConfigReloaded}, category: RunEventBookkeeping},
		{event: journal.Event{Type: journal.EventConfigReloadRejected}, category: RunEventResult, chapter: true},
		{event: journal.Event{Type: journal.EventDaemonStarted}, category: RunEventLiveness},
		{event: journal.Event{Type: journal.EventDaemonCleanShutdown}, category: RunEventLiveness},
		{event: journal.Event{Type: journal.EventDaemonDirtyRestart}, category: RunEventResult, chapter: true},
	}

	seen := make(map[journal.EventType]struct{}, len(tests))
	for _, test := range tests {
		eventType := test.event.Type
		t.Run(string(eventType), func(t *testing.T) {
			if _, duplicate := seen[eventType]; duplicate {
				t.Fatalf("duplicate classification for %q", eventType)
			}
			seen[eventType] = struct{}{}

			test.event.Schema = journal.EventSchema
			projected := projectEvent(journal.EventRecord{Event: test.event}, artifactIndex{})
			if projected.Category != test.category || projected.ReplayChapter != test.chapter {
				t.Fatalf(
					"classification = (%q, %t), want (%q, %t)",
					projected.Category,
					projected.ReplayChapter,
					test.category,
					test.chapter,
				)
			}
		})
	}
}

func TestProjectEventClassifiesExternalRefsByMeaning(t *testing.T) {
	tests := []struct {
		name        string
		externalRef *journal.ExternalRef
		category    RunEventCategory
		chapter     bool
	}{
		{
			name:        "pull request result",
			externalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "42"},
			category:    RunEventResult,
			chapter:     true,
		},
		{
			name:        "issue bookkeeping",
			externalRef: &journal.ExternalRef{Provider: "github", Kind: "issue", ID: "1426"},
			category:    RunEventBookkeeping,
		},
		{
			name:     "missing reference bookkeeping",
			category: RunEventBookkeeping,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := journal.Event{
				Schema:      journal.EventSchema,
				Type:        journal.EventRefTouched,
				ExternalRef: test.externalRef,
			}
			projected := projectEvent(journal.EventRecord{Event: event}, artifactIndex{})
			if projected.Category != test.category || projected.ReplayChapter != test.chapter {
				t.Fatalf(
					"classification = (%q, %t), want (%q, %t)",
					projected.Category,
					projected.ReplayChapter,
					test.category,
					test.chapter,
				)
			}
		})
	}
}

func TestProjectEventClassificationIsDeterministicAcrossRunOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		event    journal.Event
		category RunEventCategory
		chapter  bool
	}{
		{
			name:     "live",
			event:    journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1},
			category: RunEventTransition,
			chapter:  true,
		},
		{
			name:     "completed",
			event:    journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)},
			category: RunEventTransition,
			chapter:  true,
		},
		{
			name: "repass",
			event: journal.Event{
				Type:    journal.EventGateEvaluated,
				Verdict: "needs-changes",
				Target:  "implement",
			},
			category: RunEventDecision,
			chapter:  true,
		},
		{
			name: "retry",
			event: journal.Event{
				Type:         journal.EventStageStarted,
				Stage:        "implement",
				Attempt:      2,
				AttemptClass: journal.AttemptPolicy,
			},
			category: RunEventTransition,
			chapter:  true,
		},
		{
			name: "failed",
			event: journal.Event{
				Type:   journal.EventStageFinished,
				Status: string(apiv1.ResultFailure),
			},
			category: RunEventTransition,
			chapter:  true,
		},
		{
			name:     "aborted",
			event:    journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseAborted)},
			category: RunEventTransition,
			chapter:  true,
		},
		{
			name: "escalated",
			event: journal.Event{
				Type:      journal.EventGateEvaluated,
				Target:    workflow.TargetEscalate,
				Escalated: true,
			},
			category: RunEventDecision,
			chapter:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.event.Schema = journal.EventSchema
			record := journal.EventRecord{Event: test.event}
			first := projectEvent(record, artifactIndex{})
			second := projectEvent(record, artifactIndex{})
			if first.Category != test.category || first.ReplayChapter != test.chapter {
				t.Fatalf(
					"classification = (%q, %t), want (%q, %t)",
					first.Category,
					first.ReplayChapter,
					test.category,
					test.chapter,
				)
			}
			if second.Category != first.Category || second.ReplayChapter != first.ReplayChapter {
				t.Fatalf("repeated projection changed classification: first=%+v second=%+v", first, second)
			}
		})
	}
}

func TestProjectEventUsesStableUnknownFallback(t *testing.T) {
	tests := []journal.Event{
		{Schema: journal.EventSchema, Type: "future.event"},
		{Schema: "goobers.dev/journal/event/v99", Type: journal.EventRunFinished},
	}
	for _, event := range tests {
		projected := projectEvent(journal.EventRecord{Event: event}, artifactIndex{})
		if projected.Category != RunEventUnknown || projected.ReplayChapter {
			t.Fatalf("unknown classification = (%q, %t)", projected.Category, projected.ReplayChapter)
		}
	}
}
