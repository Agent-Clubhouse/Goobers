// Package scheduler is the Goobers system scheduler. It evaluates triggers and
// readiness conditions and, when a workflow is eligible, starts a run via the
// M7 engine's start API. A workflow is eligible IFF a trigger fires AND its
// readiness conditions are met (VISION §7, scheduler.md).
//
// Exactly-once is the engine's: the scheduler derives a deterministic RunID per
// unit of work (e.g. one per backlog item), so a duplicate dispatch is a no-op
// rather than a second run.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
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
			span.Succeed("blocked: " + c.Name())
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
		span.Succeed("already-running")
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
	in := engine.RunInput{
		RunID:        engine.RunID(s.cfg.Gaggle, def.Name, ev.DedupeKey),
		Gaggle:       s.cfg.Gaggle,
		WorkflowName: def.Name,
		Version:      def.Version,
		Spec:         def.Spec,
		RepoRef:      s.cfg.Repo,
	}
	if ev.Item != nil {
		bi := toBacklogItem(*ev.Item)
		in.Item = &bi
	}
	return in, nil
}

// startSpan opens a scheduler span for the dispatch, if telemetry is configured.
// A zero telemetry.Span is safe to use (its methods no-op), so callers need no
// nil checks. RunID is intentionally omitted: the run's OTel trace id is created
// by the engine when the run executes; correlating the two is runtime wiring.
func (s *Scheduler) startSpan(ctx context.Context, in engine.RunInput, ev Event) (context.Context, telemetry.Span) {
	if s.cfg.Telemetry == nil {
		return ctx, telemetry.Span{}
	}
	attrs := telemetry.SchedulerAttributes{
		Gaggle:          s.cfg.Gaggle,
		WorkflowID:      in.WorkflowName,
		WorkflowVersion: strconv.Itoa(in.Version),
		Action:          "evaluate",
		Reason:          ev.Reason,
	}
	if ev.Item != nil {
		attrs.ItemID = ev.Item.ID
		attrs.ItemProvider = string(ev.Item.Provider)
	}
	ctx, span, err := s.cfg.Telemetry.StartSchedulerSpan(ctx, attrs)
	if err != nil {
		return ctx, telemetry.Span{}
	}
	return ctx, span
}

// toBacklogItem maps a provider work item onto the canonical api envelope item.
func toBacklogItem(w providers.WorkItem) apiv1.BacklogItem {
	return apiv1.BacklogItem{
		ID:       w.ID,
		Provider: apiv1.Provider(w.Provider),
		Title:    w.Title,
		Body:     w.Body,
		URL:      w.URL,
		Labels:   w.Labels,
	}
}
