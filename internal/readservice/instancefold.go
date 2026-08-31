package readservice

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// instanceFold projects the instance journal for the status paths that read it
// on every request. It retains the fold between requests and asks the journal
// only for the events appended since its last read, so a request costs what the
// journal grew by rather than everything it has ever recorded (#3050).
type instanceFold struct {
	mu    sync.Mutex
	seq   uint64
	state instanceState
}

// instanceState is the scheduler and lifetime state derived from the instance
// journal: everything SchedulerStatus reports plus the earliest recorded init
// completion the time-to-first-PR metric measures from.
type instanceState struct {
	initCompletedAt       time.Time
	providerQuotaResumeAt *time.Time
	restart               *DaemonRestartStatus
	sawDaemonStart        bool
	dirtyReason           string
	refusalOrder          []string
	refusals              map[string]WorkflowRefusalStatus
	refillBlocked         map[localscheduler.WorkflowIdentity]string
}

// snapshot folds every event appended since the previous call and returns a
// copy of the resulting state, so callers can mutate what they receive without
// corrupting the retained fold.
func (f *instanceFold) snapshot(ctx context.Context, schedulerDir string) (instanceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	events, err := journal.ReadInstanceLogAfterSeq(schedulerDir, f.seq)
	if err != nil {
		return instanceState{}, err
	}
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return instanceState{}, err
		}
		f.state.apply(event)
		if event.Seq > f.seq {
			f.seq = event.Seq
		}
	}
	return f.state.clone(), nil
}

func (s *instanceState) apply(event journal.Event) {
	switch event.Type {
	case journal.EventInitCompleted:
		if !event.Time.IsZero() &&
			(s.initCompletedAt.IsZero() || event.Time.Before(s.initCompletedAt)) {
			s.initCompletedAt = event.Time
		}
	case journal.EventTriggerFired:
		if localscheduler.IsRefillTriggerReason(event.Reason) {
			delete(s.refillBlocked, localscheduler.WorkflowIdentity{Gaggle: event.Gaggle, Workflow: event.Workflow})
		}
	case journal.EventError:
		if event.Error != nil && event.Error.Code == providers.ErrorCodeAuthFailed && event.Workflow != "" {
			s.blockRefill(event.Gaggle, event.Workflow, localscheduler.ReasonProviderAuth)
		}
	case journal.EventPollShed:
		if event.Workflow != "" {
			s.blockRefill(event.Gaggle, event.Workflow, localscheduler.ReasonProviderQuota)
		}
	case journal.EventProviderQuotaReset:
		for identity, blocking := range s.refillBlocked {
			if blocking == localscheduler.ReasonProviderQuota {
				delete(s.refillBlocked, identity)
			}
		}
	case journal.EventConfigReloaded:
		// The scheduler re-journals current refusals after each accepted
		// reload, and refill blockers may have changed with the config.
		s.resetRefusals()
		s.refillBlocked = nil
	case journal.EventTickSkipped:
		if candidate, ok := parseProviderQuotaResumeTime(event.Reason); ok {
			candidate = candidate.UTC()
			s.providerQuotaResumeAt = &candidate
		}
		if blocking, ok := localscheduler.RefillBlockedReason(event.Reason); ok && event.Workflow != "" {
			s.blockRefill(event.Gaggle, event.Workflow, blocking)
		}
	case journal.EventWorkflowRefused:
		key := event.Gaggle + "/" + event.Workflow
		if _, known := s.refusals[key]; !known {
			s.refusalOrder = append(s.refusalOrder, key)
		}
		if s.refusals == nil {
			s.refusals = make(map[string]WorkflowRefusalStatus)
		}
		s.refusals[key] = WorkflowRefusalStatus{
			Gaggle:   event.Gaggle,
			Workflow: event.Workflow,
			Reason:   event.Reason,
			At:       event.Time,
		}
	case journal.EventDaemonDirtyRestart:
		s.dirtyReason = event.Reason
	case journal.EventDaemonStarted:
		s.resetRefusals()
		s.refillBlocked = nil
		if s.sawDaemonStart || s.dirtyReason != "" {
			reason := "clean restart"
			if s.dirtyReason != "" {
				reason = s.dirtyReason
			}
			s.restart = &DaemonRestartStatus{
				At:      event.Time,
				Reason:  reason,
				PID:     runnerInt(event.Runner, "pid"),
				Version: runnerString(event.Runner, "version"),
				Root:    runnerString(event.Runner, "instanceRoot"),
			}
		}
		s.sawDaemonStart = true
		s.dirtyReason = ""
	case journal.EventRunnerAnnotation:
		if s.restart != nil &&
			runnerString(event.Runner, "kind") == journal.RunnerAnnotationRunRecovery &&
			event.RunID != "" &&
			!containsString(s.restart.RunIDs, event.RunID) {
			s.restart.RunIDs = append(s.restart.RunIDs, event.RunID)
		}
	}
}

func (s *instanceState) blockRefill(gaggle, workflow, blocking string) {
	if s.refillBlocked == nil {
		s.refillBlocked = make(map[localscheduler.WorkflowIdentity]string)
	}
	s.refillBlocked[localscheduler.WorkflowIdentity{Gaggle: gaggle, Workflow: workflow}] = blocking
}

func (s *instanceState) resetRefusals() {
	s.refusalOrder = nil
	s.refusals = nil
}

func (s instanceState) clone() instanceState {
	clone := s
	if s.providerQuotaResumeAt != nil {
		resumeAt := *s.providerQuotaResumeAt
		clone.providerQuotaResumeAt = &resumeAt
	}
	if s.restart != nil {
		restart := *s.restart
		restart.RunIDs = append([]string(nil), s.restart.RunIDs...)
		restart.Replacements = append([]RunReplacement(nil), s.restart.Replacements...)
		clone.restart = &restart
	}
	clone.refusalOrder = append([]string(nil), s.refusalOrder...)
	clone.refusals = maps.Clone(s.refusals)
	clone.refillBlocked = maps.Clone(s.refillBlocked)
	return clone
}
