package readservice

import (
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
)

// Keep one latest observation per workflow, with a hard cap even if a corrupt
// or externally produced journal names unlimited workflows. Reload/start clears
// old eligibility observations; eviction removes the oldest inserted workflow.
const maxEngineFallbackWorkflows = 1024

func (s SchedulerStatus) engineFallbackFor(gaggle, workflow string) *readmodel.EngineFallback {
	for _, fallback := range s.EngineFallbacks {
		if fallback.Gaggle == gaggle && fallback.Workflow == workflow {
			return &fallback
		}
	}
	return nil
}

type engineFallbackFold struct {
	items map[localscheduler.WorkflowIdentity]readmodel.EngineFallback
	order []localscheduler.WorkflowIdentity
}

func (f *engineFallbackFold) apply(event journal.Event) {
	if event.Type == journal.EventDaemonStarted || event.Type == journal.EventConfigReloaded {
		*f = engineFallbackFold{}
		return
	}
	value, ok := readmodel.RunnerEngineFallback(event)
	if !ok || value.Workflow == "" {
		return
	}
	identity := localscheduler.WorkflowIdentity{Gaggle: value.Gaggle, Workflow: value.Workflow}
	if f.items == nil {
		f.items = make(map[localscheduler.WorkflowIdentity]readmodel.EngineFallback)
	}
	if _, exists := f.items[identity]; !exists {
		if len(f.order) >= maxEngineFallbackWorkflows {
			delete(f.items, f.order[0])
			f.order = f.order[1:]
		}
		f.order = append(f.order, identity)
	}
	f.items[identity] = *value
}

func (f engineFallbackFold) clone() engineFallbackFold {
	out := engineFallbackFold{order: append([]localscheduler.WorkflowIdentity(nil), f.order...)}
	if f.items != nil {
		out.items = make(map[localscheduler.WorkflowIdentity]readmodel.EngineFallback, len(f.items))
	}
	for key, value := range f.items {
		value.SelfPinnedStages = append([]string(nil), value.SelfPinnedStages...)
		value.UnpinnedGates = append([]string(nil), value.UnpinnedGates...)
		out.items[key] = value
	}
	return out
}
