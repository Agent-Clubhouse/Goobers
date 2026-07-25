package readservice

import "github.com/goobers/goobers/internal/journal"

// RunEventCategory is presentation metadata describing an event's replay role.
type RunEventCategory string

// RunEventTransition through RunEventUnknown are the bounded replay categories.
const (
	RunEventTransition  RunEventCategory = "transition"
	RunEventDecision    RunEventCategory = "decision"
	RunEventResult      RunEventCategory = "result"
	RunEventEvidence    RunEventCategory = "evidence"
	RunEventLiveness    RunEventCategory = "liveness"
	RunEventBookkeeping RunEventCategory = "bookkeeping"
	RunEventUnknown     RunEventCategory = "unknown"
)

func classifyRunEvent(event journal.Event) (RunEventCategory, bool) {
	if !event.KnownSchema() {
		return RunEventUnknown, false
	}

	switch event.Type {
	case journal.EventRunStarted,
		journal.EventRunResumed,
		journal.EventRunFinished,
		journal.EventStageStarted,
		journal.EventStageFinished,
		journal.EventStageRerunRequested,
		journal.EventGatePaused,
		journal.EventTriggerFired:
		return RunEventTransition, true

	case journal.EventGateEvaluated,
		journal.EventTickSkipped:
		return RunEventDecision, true

	case journal.EventError,
		journal.EventWorkflowStarved,
		journal.EventClaimLockTimeout,
		journal.EventConfigReloadRejected,
		journal.EventDaemonDirtyRestart:
		return RunEventResult, true

	case journal.EventArtifactRecorded,
		journal.EventSpanRecorded,
		journal.EventInputSnapshot:
		return RunEventEvidence, false

	case journal.EventStageHeartbeat,
		journal.EventProviderQuotaReset,
		journal.EventPollShed,
		journal.EventDaemonStarted,
		journal.EventDaemonCleanShutdown:
		return RunEventLiveness, false

	case journal.EventGateStarted,
		journal.EventRedaction,
		journal.EventRepaired,
		journal.EventRunnerAnnotation,
		journal.EventClaimAcquired,
		journal.EventClaimReleased,
		journal.EventClaimForceReleased,
		journal.EventClaimLockSlow,
		journal.EventConfigReloaded:
		return RunEventBookkeeping, false

	case journal.EventRefTouched:
		if event.ExternalRef != nil && event.ExternalRef.Kind == "pr" {
			return RunEventResult, true
		}
		return RunEventBookkeeping, false

	default:
		return RunEventUnknown, false
	}
}
