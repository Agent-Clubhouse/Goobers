// Package scheduler is the Goobers system scheduler. It evaluates triggers and
// readiness conditions and, when a workflow is eligible, starts a run via the
// M7 engine's start API. A workflow is eligible IFF a trigger fires AND its
// readiness conditions are met (VISION §7, scheduler.md).
//
// Exactly-once is the engine's: the scheduler derives a deterministic RunID per
// unit of work (e.g. one per backlog item), so a duplicate dispatch is a no-op
// rather than a second run.
//
// Tier-3 (V2) — quarantined, not on the V0 path. See docs/ARCHITECTURE.md §11.
// Revived in V2. The V0-live scheduler is internal/localscheduler.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/backlog"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

// Event is a candidate run surfaced by a trigger.
type Event struct {
	// WorkflowName is the workflow this event would start.
	WorkflowName string
	// Item is the backlog item that triggered this event, if any (nil for
	// schedule/signal triggers).
	Item *providers.WorkItem
	// Reason is a short description of why the trigger fired (for telemetry).
	Reason string
	// DedupeKey is the identity used to make starting idempotent. For a backlog
	// item it is the item id; for a schedule it is a per-fire token. Two events
	// with the same (workflow, dedupe key) resolve to the same run and never
	// double-start.
	DedupeKey string
}

// trigger derives the run's pinned trigger identity from the event shape: an
// item-carrying event (backlog poll, provider event) is an item trigger, a
// bare tick a schedule fire — journal.TriggerKind vocabulary, which also
// drives the engine's deferred branch-provenance rule (#629). Ref is the
// event's dedupe key, the same identity the deterministic RunID is minted
// from.
func (ev Event) trigger() journal.Trigger {
	kind := journal.TriggerSchedule
	if ev.Item != nil {
		kind = journal.TriggerItem
	}
	return journal.Trigger{Kind: kind, Ref: ev.DedupeKey}
}

// Decision is the outcome of dispatching an Event.
type Decision struct {
	// Started is true when this dispatch began a new run.
	Started bool
	// RunID is the run's id (set when Started, or when a run was already running).
	RunID string
	// Reason explains why a run did not start (readiness blocked, already running).
	Reason string
}

// SpanStarter is the slice of the telemetry client the scheduler needs.
// *telemetry.Client satisfies it.
type SpanStarter interface {
	StartSchedulerSpan(ctx context.Context, attrs telemetry.SchedulerAttributes) (context.Context, telemetry.Span, error)
}

// Config configures a gaggle-scoped Scheduler.
type Config struct {
	// Gaggle this scheduler serves.
	Gaggle string
	// Repo is the gaggle's project repository, seeded into each run.
	Repo apiv1.RepoRef
	// Registry resolves workflow definitions (and pins versions).
	Registry *engine.Registry
	// Starter begins runs (the engine start API).
	Starter engine.Starter
	// Readiness conditions that must all pass before a run starts.
	Readiness []ReadinessCondition
	// Claimer optionally mirrors a backlog item's status when claimed (humans
	// only; the authoritative claim is the engine's deterministic RunID).
	Claimer Claimer
	// Telemetry optionally records a scheduler span per dispatch.
	Telemetry SpanStarter
	// BranchNamespace is the gaggle's run-branch namespace root
	// (GaggleSpec.BranchNamespace, #1109), pinned into every run this
	// scheduler starts. Empty means the default namespace.
	BranchNamespace string
	// GateGooberCapabilities maps a reviewer goober name to its declared
	// capability grants (#294) — the pinned lookup an agentic gate's envelope
	// draws from, since AgenticGate carries no stage-level capabilities.
	// bootstrap derives it from the loaded Goober definitions.
	GateGooberCapabilities map[string][]string
	// MaxRepasses overrides the shared gate repass budget when > 0
	// (gate.DefaultMaxRepasses applies otherwise), pinned per run like the
	// local runner's Config.MaxRepasses.
	MaxRepasses int
}

// Scheduler decides when to start workflow runs for one gaggle.
type Scheduler struct {
	cfg Config
}

// New validates cfg and returns a Scheduler.
func New(cfg Config) (*Scheduler, error) {
	if cfg.Gaggle == "" {
		return nil, errors.New("scheduler: gaggle is required")
	}
	if cfg.Registry == nil {
		return nil, errors.New("scheduler: registry is required")
	}
	if cfg.Starter == nil {
		return nil, errors.New("scheduler: starter is required")
	}
	return &Scheduler{cfg: cfg}, nil
}

// Dispatch evaluates one event: it checks readiness, then (if ready) claims and
// starts the run. It never starts a run that is already in flight.
func (s *Scheduler) Dispatch(ctx context.Context, ev Event) (Decision, error) {
	if ev.WorkflowName == "" {
		return Decision{}, errors.New("scheduler: event has no workflow name")
	}
	in, err := s.buildRunInput(ev)
	if err != nil {
		return Decision{}, err
	}

	ctx, span := s.startSpan(ctx, in, ev)
	defer span.End()

	for _, c := range s.cfg.Readiness {
		ready, reason, rerr := c.Ready(ctx, ev)
		if rerr != nil {
			span.Fail(rerr)
			return Decision{}, fmt.Errorf("readiness %q: %w", c.Name(), rerr)
		}
		if !ready {
			span.Event("blocked")
			span.Complete(telemetry.OutcomeBlocked, false)
			return Decision{Started: false, Reason: c.Name() + ": " + reason}, nil
		}
	}

	// Mirror the claim to the backlog for humans; non-fatal — the authoritative
	// exactly-once claim is the deterministic RunID handled by Start below.
	if ev.Item != nil && s.cfg.Claimer != nil {
		if cerr := s.cfg.Claimer.Claim(ctx, *ev.Item); cerr != nil {
			span.Event("claim-mirror-failed")
		}
	}

	res, err := s.cfg.Starter.Start(ctx, in)
	if err != nil {
		span.Fail(err)
		return Decision{}, fmt.Errorf("start run %q: %w", in.RunID, err)
	}
	if res.AlreadyRunning {
		span.Complete(telemetry.OutcomeBlocked, false)
		return Decision{Started: false, RunID: res.RunID, Reason: "run already in flight"}, nil
	}
	span.Succeed("started")
	return Decision{Started: true, RunID: res.RunID}, nil
}

func (s *Scheduler) buildRunInput(ev Event) (engine.RunInput, error) {
	def, ok := s.cfg.Registry.Latest(ev.WorkflowName)
	if !ok {
		return engine.RunInput{}, fmt.Errorf("scheduler: workflow %q is not registered", ev.WorkflowName)
	}
	machine, err := s.cfg.Registry.Compile(def)
	if err != nil {
		return engine.RunInput{}, fmt.Errorf("scheduler: compile pinned workflow %q: %w", ev.WorkflowName, err)
	}
	allowPreviewFeatures := s.cfg.Registry.PreviewFeaturesEnabled()
	trigger := ev.trigger()
	in := engine.RunInput{
		RunID:                  engine.RunID(s.cfg.Gaggle, def.Name, ev.DedupeKey),
		Gaggle:                 s.cfg.Gaggle,
		WorkflowName:           def.Name,
		Version:                def.Version,
		DSLVersion:             def.DSLVersion,
		WorkflowDigest:         machine.Digest(),
		PreviewFeaturesEnabled: &allowPreviewFeatures,
		Spec:                   def.Spec,
		RepoRef:                s.cfg.Repo,
		TriggerKind:            string(trigger.Kind),
		TriggerRef:             trigger.Ref,
		BranchNamespace:        s.cfg.BranchNamespace,
		GateGooberCapabilities: s.cfg.GateGooberCapabilities,
		MaxRepasses:            s.cfg.MaxRepasses,
	}
	if ev.Item != nil {
		bi := backlog.FromWorkItem(*ev.Item)
		in.Item = &bi
	}
	return in, nil
}

// startSpan opens a scheduler span for the dispatch, if telemetry is configured.
// A zero telemetry.Span is safe to use (its methods no-op), so callers need no
// nil checks. The deterministic run ID is also the trace ID, correlating this
// decision with the run before the engine starts it.
func (s *Scheduler) startSpan(ctx context.Context, in engine.RunInput, ev Event) (context.Context, telemetry.Span) {
	if s.cfg.Telemetry == nil {
		return ctx, telemetry.Span{}
	}
	attrs := telemetry.SchedulerAttributes{
		Gaggle:          s.cfg.Gaggle,
		WorkflowID:      in.WorkflowName,
		WorkflowVersion: strconv.Itoa(in.Version),
		WorkflowDigest:  in.WorkflowDigest,
		RunID:           in.RunID,
		Action:          "evaluate",
	}
	if ev.Item != nil {
		attrs.ItemID = ev.Item.ID
		attrs.ItemURL = ev.Item.URL
	}
	ctx, span, err := s.cfg.Telemetry.StartSchedulerSpan(ctx, attrs)
	if err != nil {
		return ctx, telemetry.Span{}
	}
	return ctx, span
}
