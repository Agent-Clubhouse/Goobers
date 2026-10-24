// Package schedentries converts the applied cloud generation into the entries
// the shared scheduler ticks, and builds the schedule/occurrence intake over the
// cloud acceptance store. It is runner-free: the cloud control plane only admits
// engine-eligible generations and never executes a stage.
//
// What it reuses from the local daemon is the pure part of its entry builder
// (trigger parsing, readiness, repository identity); everything runner-bound
// (per-gaggle runners, worktrees, local credentials, harness preflight) has no
// cloud counterpart and is deliberately absent rather than stubbed.
package schedentries

import (
	"errors"
	"fmt"
	"sort"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

// workflowVersion is the compiled workflow version every start pins; the cloud
// identifies a workflow by digest, as the local runner wiring does.
const workflowVersion = 1

// BacklogPendingReason disables a workflow whose backlog-item trigger cannot be
// served yet. It is an explicit, named refusal: the entry exists, every run
// attempt is refused with this diagnostic, and nothing fails silently.
const BacklogPendingReason = "backlog trigger pending cloud provider counters (L1d)"

// Input is the applied generation and the cloud starter factory.
type Input struct {
	// Generation is the applied generation's digest (entry.ConfigGeneration).
	Generation string
	// Config is the applied policy configuration (instance.yaml).
	Config *instance.Config
	// Set is the applied definitions, the same set the pins were computed on.
	Set *instance.ConfigSet
	// Pins are the admission digests keyed "gaggle/workflow" (Candidate.Pins).
	Pins map[string]cloudconfig.Pins
	// Starter returns the cloud starter for one workflow. It must not be nil for
	// a workflow the generation contains.
	Starter func(localscheduler.WorkflowIdentity) localscheduler.Starter
}

// Validate reports whether every enabled trigger of every workflow parses. The
// config loader calls it at apply time so an unparseable trigger refuses the
// generation (naming the workflow) instead of failing at tick time.
func Validate(cfg *instance.Config, set *instance.ConfigSet) error {
	if cfg == nil || set == nil {
		return errors.New("schedentries: a loaded configuration is required")
	}
	loc, err := cfg.Location()
	if err != nil {
		return err
	}
	var problems []error
	for i := range set.Workflows {
		if _, err := triggers(&set.Workflows[i], loc); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

// Build returns one entry per workflow of the applied generation, in a stable
// order. A workflow with no pins is an error: admitting an unpinned workflow
// would let a start run against whatever is applied later.
func Build(in Input) ([]localscheduler.WorkflowEntry, error) {
	if in.Generation == "" || in.Config == nil || in.Set == nil || in.Starter == nil {
		return nil, errors.New("schedentries: generation, configuration, definitions and a starter factory are required")
	}
	loc, err := in.Config.Location()
	if err != nil {
		return nil, err
	}
	gaggles := make(map[string]apiv1.Gaggle, len(in.Set.Gaggles))
	for _, gaggle := range in.Set.Gaggles {
		gaggles[gaggle.Name] = gaggle
	}
	entries := make([]localscheduler.WorkflowEntry, 0, len(in.Set.Workflows))
	for i := range in.Set.Workflows {
		wf := &in.Set.Workflows[i]
		gaggle, ok := gaggles[wf.Spec.Gaggle]
		if !ok {
			return nil, fmt.Errorf("workflow %q references unknown gaggle %q", wf.Name, wf.Spec.Gaggle)
		}
		identity := localscheduler.WorkflowIdentity{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name}
		pins, ok := in.Pins[wf.Spec.Gaggle+"/"+wf.Name]
		if !ok || pins.WorkflowDigest == "" || pins.GooberDigest == "" {
			return nil, fmt.Errorf("workflow %q (gaggle %q) has no admission pins in generation %s", wf.Name, wf.Spec.Gaggle, in.Generation)
		}
		starter := in.Starter(identity)
		if starter == nil {
			return nil, fmt.Errorf("workflow %q (gaggle %q) has no starter", wf.Name, wf.Spec.Gaggle)
		}
		t, err := triggers(wf, loc)
		if err != nil {
			return nil, err
		}
		entry := localscheduler.WorkflowEntry{
			ConfigGeneration:     in.Generation,
			Workflow:             wf.Name,
			WorkflowVersion:      workflowVersion,
			WorkflowDigest:       pins.WorkflowDigest,
			GooberDigest:         pins.GooberDigest,
			Gaggle:               wf.Spec.Gaggle,
			Readiness:            wf.Spec.Readiness,
			Schedules:            t.schedules,
			ScheduleCatchUpAll:   wf.Spec.ScheduleCatchUp == "all",
			ScheduleBackoffs:     t.scheduleBackoffs,
			WebhookBackoff:       t.webhookBackoff,
			Signals:              t.signals,
			PollProvider:         apiv1.ProviderGitHub,
			PollPriority:         t.pollPriority,
			Starter:              starter,
			RepoRef:              gaggle.Spec.Project,
			RequiredCapabilities: instance.WorkflowRequiredCapabilities(gaggle, *wf),
			DisabledReason:       disabledReason(gaggle, wf, t.backlog),
		}
		if t.hasRepositoryWebhook && len(t.schedules) > 0 {
			entry.PollFallbackCause = pollFallbackCause(in.Config, gaggle)
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Gaggle != entries[j].Gaggle {
			return entries[i].Gaggle < entries[j].Gaggle
		}
		return entries[i].Workflow < entries[j].Workflow
	})
	return entries, nil
}

type parsedTriggers struct {
	schedules            []localscheduler.Schedule
	scheduleBackoffs     []localscheduler.IdleBackoffConfig
	signals              []string
	webhookBackoff       localscheduler.IdleBackoffConfig
	hasRepositoryWebhook bool
	backlog              bool
	pollPriority         int32
}

// triggers parses the workflow's enabled triggers, as the local daemon does.
func triggers(wf *apiv1.Workflow, loc *time.Location) (parsedTriggers, error) {
	out := parsedTriggers{}
	out.webhookBackoff, _ = localscheduler.ParseIdleBackoff(nil)
	prioritySet := false
	for _, trigger := range wf.Spec.Triggers {
		if triggerDisabled(trigger) {
			continue
		}
		switch trigger.Type {
		case apiv1.TriggerSchedule:
			if trigger.Schedule != "" {
				schedule, err := localscheduler.ParseSchedule(trigger.Schedule)
				if err != nil {
					return parsedTriggers{}, fmt.Errorf("workflow %q: %w", wf.Name, err)
				}
				backoff, err := localscheduler.ParseIdleBackoff(trigger.IdleBackoff)
				if err != nil {
					return parsedTriggers{}, fmt.Errorf("workflow %q: %w", wf.Name, err)
				}
				out.schedules = append(out.schedules, localscheduler.InLocation(schedule, loc))
				out.scheduleBackoffs = append(out.scheduleBackoffs, backoff)
			}
		case apiv1.TriggerSignal:
			if trigger.Signal != "" {
				out.signals = append(out.signals, trigger.Signal)
			}
		case apiv1.TriggerWebhook:
			out.hasRepositoryWebhook = true
			backoff, err := localscheduler.ParseIdleBackoff(trigger.IdleBackoff)
			if err != nil {
				return parsedTriggers{}, fmt.Errorf("workflow %q: %w", wf.Name, err)
			}
			for _, event := range trigger.Events {
				out.signals = append(out.signals, webhookhttp.SignalName(event))
			}
			out.webhookBackoff = backoff
		case apiv1.TriggerBacklogItem:
			out.backlog = true
		}
		if trigger.Type == apiv1.TriggerBacklogItem || trigger.Type == apiv1.TriggerSchedule {
			if !prioritySet || trigger.Priority > out.pollPriority {
				out.pollPriority, prioritySet = trigger.Priority, true
			}
		}
	}
	return out, nil
}

// triggerDisabled reports whether Trigger.Enabled explicitly suppresses a
// non-manual trigger (nil keeps the historical enabled behavior).
func triggerDisabled(trigger apiv1.Trigger) bool {
	if trigger.Enabled == nil || *trigger.Enabled {
		return false
	}
	return trigger.Type != apiv1.TriggerManual
}

func disabledReason(gaggle apiv1.Gaggle, wf *apiv1.Workflow, backlog bool) string {
	switch {
	case gaggle.Spec.Enabled != nil && !*gaggle.Spec.Enabled:
		return fmt.Sprintf("gaggle %q is disabled (spec.enabled=false)", gaggle.Name)
	case wf.Spec.Enabled != nil && !*wf.Spec.Enabled:
		return fmt.Sprintf("workflow %q is disabled (spec.enabled=false)", wf.Name)
	case backlog:
		return BacklogPendingReason
	}
	return ""
}

func pollFallbackCause(cfg *instance.Config, gaggle apiv1.Gaggle) string {
	switch {
	case gaggle.Spec.Project.Provider != apiv1.ProviderGitHub:
		return "repository provider does not support GitHub webhook delivery"
	case !cfg.WebhookSecretConfigured():
		return "webhook listener is disabled because webhook.secret is not configured"
	default:
		return "no usable webhook delivery was available"
	}
}
