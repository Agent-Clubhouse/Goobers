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
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runtimewiring"
	webhookhttp "github.com/goobers/goobers/internal/webhook"
)

// workflowVersion is the compiled workflow version every start pins; the cloud
// identifies a workflow by digest, as the local runner wiring does.
const workflowVersion = 1

// The provider-backed demand counters (L1d) do not exist in the cloud yet. A
// workflow that needs one is not left to misbehave silently (a schedule that
// fires every tick with no work, a desired-concurrency target that never
// refills, a backlog trigger that never polls): its entry exists, and every run
// attempt is refused with a named diagnostic.
const (
	// BacklogPendingReason: the workflow has a backlog-item trigger.
	BacklogPendingReason = "backlog trigger pending cloud provider counters (L1d)"
	// ScheduleDemandPendingReason: a scheduled remediation workflow is sized to
	// its eligible work locally; unsized, it would start a run every tick.
	ScheduleDemandPendingReason = "schedule demand counter pending cloud provider counters (L1d)"
	// RefillPendingReason: Readiness.DesiredConcurrentRuns refills from provider
	// eligibility; without a counter it would silently never refill.
	RefillPendingReason = "desired-concurrency refill pending cloud provider counters (L1d)"
)

// ErrNoStarter is what a start returns when no cloud starter was supplied for
// the workflow: the entry can be built and inspected, but never starts a run.
var ErrNoStarter = errors.New("schedentries: no cloud starter is configured for this workflow")

type unavailableStarter struct{}

func (unavailableStarter) Start(context.Context, localscheduler.StartRequest) (localscheduler.StartResult, error) {
	return localscheduler.StartResult{}, ErrNoStarter
}

// Counters are the provider-backed demand counters of one workflow (L1d). A nil
// counter means none exists for that role, and a workflow that needs it is
// disabled with the matching pending reason rather than left to misbehave.
type Counters struct {
	// Backlog sizes a backlog-item trigger's fan-out.
	Backlog localscheduler.BacklogCounter
	// ScheduleDemand sizes a due scheduled-remediation fire to eligible work.
	ScheduleDemand localscheduler.BacklogCounter
	// RefillDemand reports eligibility for desired-concurrency refill.
	RefillDemand localscheduler.BacklogCounter
}

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
	// Starter returns the cloud starter for one workflow. A nil factory (or a nil
	// result) yields a starter that refuses every start with ErrNoStarter, so
	// entries can be built without a real starter and fail loudly if ticked.
	Starter func(localscheduler.WorkflowIdentity) localscheduler.Starter
	// Counters supplies the provider-backed counters (L1d). nil, or a zero
	// Counters for a workflow, leaves that workflow's counter-dependent behavior
	// explicitly disabled.
	Counters func(localscheduler.WorkflowIdentity) Counters
}

// CodeTrigger is the finding code for a trigger the scheduler cannot parse.
const CodeTrigger = "TRIGGER001"

// Assess reports, per "gaggle/workflow", every workflow whose enabled triggers
// cannot be parsed (an invalid cron expression or idle backoff, or an instance
// time zone that does not load, which blocks every workflow). It is an
// engine-only-admission style assessor: composed with the readiness assessor, an
// unparseable trigger refuses the whole generation at apply time and `config
// status` names the workflow, instead of failing at tick time.
func Assess(cfg *instance.Config, set *instance.ConfigSet) cloudconfig.Assessment {
	out := cloudconfig.Assessment{}
	if cfg == nil || set == nil {
		return out
	}
	loc, locErr := cfg.Location()
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		var err error
		if locErr != nil {
			// Build cannot construct any entry without the zone, so every workflow
			// is blocked, not just the scheduled ones.
			err = locErr
		} else {
			_, err = triggers(wf, loc)
		}
		if err == nil {
			continue
		}
		key := wf.Spec.Gaggle + "/" + wf.Name
		out[key] = append(out[key], cloudconfig.Finding{
			Code: CodeTrigger, Path: "/spec/triggers", Message: err.Error(),
			Unblock: "fix the workflow's trigger (cron expression, idle backoff) or the instance time zone, then re-apply",
		})
	}
	return out
}

// Validate is Assess flattened to an error naming every offending workflow.
func Validate(cfg *instance.Config, set *instance.ConfigSet) error {
	if cfg == nil || set == nil {
		return errors.New("schedentries: a loaded configuration is required")
	}
	var problems []error
	for key, findings := range Assess(cfg, set) {
		for _, f := range findings {
			problems = append(problems, fmt.Errorf("%s: %s", key, f.Message))
		}
	}
	return errors.Join(problems...)
}

// Build returns one entry per workflow of the applied generation, in a stable
// order. A workflow with no pins is an error: admitting an unpinned workflow
// would let a start run against whatever is applied later.
func Build(in Input) ([]localscheduler.WorkflowEntry, error) {
	if in.Generation == "" || in.Config == nil || in.Set == nil {
		return nil, errors.New("schedentries: generation, configuration and definitions are required")
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
		var starter localscheduler.Starter = unavailableStarter{}
		if in.Starter != nil {
			if built := in.Starter(identity); built != nil {
				starter = built
			}
		}
		t, err := triggers(wf, loc)
		if err != nil {
			return nil, err
		}
		var counters Counters
		if in.Counters != nil {
			counters = in.Counters(identity)
		}
		entry := localscheduler.WorkflowEntry{
			ConfigGeneration:      in.Generation,
			Workflow:              wf.Name,
			WorkflowVersion:       workflowVersion,
			WorkflowDigest:        pins.WorkflowDigest,
			GooberDigest:          pins.GooberDigest,
			Gaggle:                wf.Spec.Gaggle,
			Readiness:             wf.Spec.Readiness,
			Schedules:             t.schedules,
			ScheduleCatchUpAll:    wf.Spec.ScheduleCatchUp == "all",
			ScheduleBackoffs:      t.scheduleBackoffs,
			WebhookBackoff:        t.webhookBackoff,
			Signals:               t.signals,
			PollProvider:          apiv1.ProviderGitHub,
			PollPriority:          t.pollPriority,
			Starter:               starter,
			RepoRef:               gaggle.Spec.Project,
			RequiredCapabilities:  instance.WorkflowRequiredCapabilities(gaggle, *wf),
			BacklogCounter:        counters.Backlog,
			ScheduleDemandCounter: counters.ScheduleDemand,
			RefillDemandCounter:   counters.RefillDemand,
			DisabledReason:        disabledReason(in.Config, gaggle, wf, t, counters),
			// ProviderAuth is deliberately unset: the local gate checks the
			// controller's own provider credentials before autonomous dispatch, but
			// the cloud control plane runs no stage and holds no stage credentials.
			// Provider authorization is exercised by the worker that runs the stage.
			//
			// HarnessRefusal and PlacementRefusal are also unset. The applied
			// generation is engine-only by construction and engine placement is
			// pinned at start.
			// TODO(L1c): surface an engine-unplaceable workflow here as a
			// DisabledReason, so it is refused once instead of failing to resolve on
			// every sweep.
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
	// backlogDeclared is true for any backlog-item trigger, enabled or not: the
	// refill poll is suppressed by the declaration, as locally.
	backlogDeclared bool
	pollPriority    int32
}

// triggers parses the workflow's enabled triggers, as the local daemon does.
func triggers(wf *apiv1.Workflow, loc *time.Location) (parsedTriggers, error) {
	out := parsedTriggers{}
	out.webhookBackoff, _ = localscheduler.ParseIdleBackoff(nil)
	prioritySet := false
	for _, trigger := range wf.Spec.Triggers {
		if trigger.Type == apiv1.TriggerBacklogItem {
			out.backlogDeclared = true
		}
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

func disabledReason(cfg *instance.Config, gaggle apiv1.Gaggle, wf *apiv1.Workflow, t parsedTriggers, counters Counters) string {
	switch {
	case gaggle.Spec.Enabled != nil && !*gaggle.Spec.Enabled:
		return fmt.Sprintf("gaggle %q is disabled (spec.enabled=false)", gaggle.Name)
	case wf.Spec.Enabled != nil && !*wf.Spec.Enabled:
		return fmt.Sprintf("workflow %q is disabled (spec.enabled=false)", wf.Name)
	}
	var pending []string
	if t.backlog && counters.Backlog == nil {
		pending = append(pending, BacklogPendingReason)
	}
	if counters.ScheduleDemand == nil && needsScheduleDemand(cfg, gaggle, wf, t) {
		pending = append(pending, ScheduleDemandPendingReason)
	}
	if counters.RefillDemand == nil && needsRefill(cfg, wf, t) {
		pending = append(pending, RefillPendingReason)
	}
	return strings.Join(pending, "; ")
}

// needsScheduleDemand mirrors when the local daemon builds a schedule demand
// counter: a scheduled workflow whose first stage is the built-in
// update-behind-pr remediation, with configured repositories. Azure DevOps has
// no counter locally either (each due tick fires unsized there too).
func needsScheduleDemand(cfg *instance.Config, gaggle apiv1.Gaggle, wf *apiv1.Workflow, t parsedTriggers) bool {
	if len(cfg.Repos) == 0 || len(t.schedules) == 0 || counterProvider(cfg, gaggle.Spec.Project) == apiv1.ProviderADO {
		return false
	}
	return startsWithGoobersCommand(wf, "update-behind-pr", func(n int) bool { return n == 2 })
}

// counterProvider resolves the provider a demand counter would talk to exactly
// as the local daemon's backlogCounterRepoRef does: the configured repository
// bound to the gaggle's project wins over the gaggle's own (possibly omitted)
// provider, an older gaggle that omits it is matched by repository identity, and
// the default is GitHub.
func counterProvider(cfg *instance.Config, project apiv1.RepoRef) apiv1.Provider {
	provider := project.Provider
	if configured, ok := runtimewiring.ConfiguredRepoForProject(cfg, project); ok && configured.Provider != "" {
		provider = apiv1.Provider(configured.Provider)
	} else if cfg != nil {
		for _, configured := range cfg.Repos {
			if configured.Owner == project.Owner && configured.Project == project.Project && configured.Name == project.Name {
				if configured.Provider != "" {
					provider = apiv1.Provider(configured.Provider)
				}
				break
			}
		}
	}
	if provider == "" {
		provider = apiv1.ProviderGitHub
	}
	return provider
}

// needsRefill mirrors when the local daemon builds a refill demand counter: a
// desired occupancy, configured repositories, no backlog trigger (which already
// polls), and a first stage that is the built-in backlog-query.
func needsRefill(cfg *instance.Config, wf *apiv1.Workflow, t parsedTriggers) bool {
	if wf.Spec.Readiness.DesiredConcurrentRuns <= 0 || len(cfg.Repos) == 0 || t.backlogDeclared {
		return false
	}
	return startsWithGoobersCommand(wf, "backlog-query", func(n int) bool { return n >= 2 })
}

func startsWithGoobersCommand(wf *apiv1.Workflow, name string, arity func(int) bool) bool {
	for _, task := range wf.Spec.Tasks {
		if task.Name == wf.Spec.Start && task.Run != nil && arity(len(task.Run.Command)) &&
			task.Run.Command[0] == "goobers" && task.Run.Command[1] == name {
			return true
		}
	}
	return false
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
