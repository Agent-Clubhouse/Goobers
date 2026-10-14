package localscheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// HarnessRecheck re-evaluates a startup harness refusal that was transient
// (#6017). HarnessRefusal returns the refusal that holds at now, or "" once
// the harness is usable again. It is consulted on every scheduler pass, so it
// must answer without blocking; any re-probe it starts runs in the background
// and must end when ctx — the scheduler's lifetime — is done.
type HarnessRecheck interface {
	HarnessRefusal(ctx context.Context, now time.Time) string
}

// harnessRecheckState is the scheduler's view of its transient harness
// refusals: the refusal last journaled for each workflow, so a change —
// re-admission, a new next-probe time, or a re-probe that found a real
// sign-in failure — reaches the instance journal and `goobers status`.
type harnessRecheckState struct {
	lifetime atomic.Pointer[lifetimeContext]

	mu       sync.Mutex
	reported map[WorkflowIdentity]string
}

type lifetimeContext struct{ context.Context }

func (h *harnessRecheckState) setLifetime(ctx context.Context) {
	h.lifetime.Store(&lifetimeContext{ctx})
}

// context is the scheduler's lifetime: Run's context once it is running.
func (h *harnessRecheckState) context() context.Context {
	if held := h.lifetime.Load(); held != nil {
		return held.Context
	}
	return context.Background()
}

// reset forgets every reported refusal: the scheduler has just learned a
// configuration and journaled its refusals afresh.
func (h *harnessRecheckState) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reported = nil
}

// changed records refusal as identity's current one and reports whether it
// differs from what was last reported (initially, the startup refusal).
func (h *harnessRecheckState) changed(identity WorkflowIdentity, startup, refusal string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	last, seen := h.reported[identity]
	if !seen {
		last = startup
	}
	if refusal == last {
		return false
	}
	if h.reported == nil {
		h.reported = make(map[WorkflowIdentity]string)
	}
	h.reported[identity] = refusal
	return true
}

// currentHarnessRefusal is entry's harness refusal at now. A transient
// refusal is re-evaluated through its recheck, and any change is journaled:
// workflow.readmitted when it clears, workflow.refused with the new reason
// otherwise.
func (s *Scheduler) currentHarnessRefusal(entry WorkflowEntry, now time.Time) string {
	if entry.HarnessRefusal == "" || entry.HarnessRecheck == nil {
		return entry.HarnessRefusal
	}
	refusal := entry.HarnessRecheck.HarnessRefusal(s.harnessRechecks.context(), now)
	if !s.harnessRechecks.changed(entryIdentity(entry), entry.HarnessRefusal, refusal) {
		return refusal
	}
	event := journal.Event{Type: journal.EventWorkflowRefused, Workflow: entry.Workflow, Gaggle: entry.Gaggle, Reason: refusal}
	if refusal == "" {
		event.Type = journal.EventWorkflowReadmitted
		event.Reason = "harness preflight passed after a transient provider failure"
	}
	s.journalEvent(event)
	return refusal
}
