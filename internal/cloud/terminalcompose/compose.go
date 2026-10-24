// Package terminalcompose assembles one lease epoch's terminal-hook machinery
// (#7818 group D): the leader-owned awaiter that fires the seven terminal hooks
// for each finished engine run, the PostgreSQL state they need, and the delivery
// loop that drains the terminal-report outbox. It is the only place the shared,
// runner-free policies (escalationnotify, parking, prepare, settle) meet the
// cloud's stores and its provider writer.
//
// Everything here is bound to one acquired epoch's lease handle. The process
// composition root supplies the dependencies as plain values and funcs; this
// package reads no flags and owns no process state.
package terminalcompose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/capability"

	"github.com/goobers/goobers/internal/cloud/blockedrecordspg"
	"github.com/goobers/goobers/internal/cloud/failurestreakpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/noworkstreakpg"
	"github.com/goobers/goobers/internal/cloud/terminalawait"
	"github.com/goobers/goobers/internal/cloud/terminalframedpg"
	"github.com/goobers/goobers/internal/cloud/terminalreportpg"
	"github.com/goobers/goobers/internal/cloud/terminalstate"
	"github.com/goobers/goobers/internal/cloud/terminalwatermarkpg"
	"github.com/goobers/goobers/internal/cloud/terminalwriter"
	"github.com/goobers/goobers/internal/escalationnotify"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalhooks"
	"github.com/goobers/goobers/internal/terminalhooks/frame"
	"github.com/goobers/goobers/internal/terminalhooks/parking"
	"github.com/goobers/goobers/internal/terminalhooks/providerposter"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

// Tuning holds the timing knobs; zero values take the documented defaults.
type Tuning struct {
	// LagBound, Margin, ClosedEvery, MaxConcurrent, RetryBase and RetryMax are the
	// awaiter's (see terminalawait.Config).
	LagBound      time.Duration
	Margin        time.Duration
	ClosedEvery   time.Duration
	MaxConcurrent int
	RetryBase     time.Duration
	RetryMax      time.Duration
	// HealEvery is how often the blocked-record self-heal verifies items with the
	// provider (default DefaultHealEvery, minutes).
	HealEvery time.Duration
	// DeliveryEvery is how often the outbox delivery loop runs (default 5s, with
	// 20% jitter), DeliveryBudget the intents it delivers per pass (default 20)
	// and HealLimit the items the blocked-record self-heal verifies per pass
	// (default 20).
	DeliveryEvery  time.Duration
	DeliveryBudget int
	HealLimit      int
	// DeferralBound is the longest a blocked-record cycle that cannot be verified may
	// wait before it is escalated as unverifiable (default and minimum clamp: see
	// blockedrecordspg.ClampDeferralBound). SweepCycles and SweepMembers bound the
	// cycles re-checked and the provider reads made per sweep pass.
	DeferralBound time.Duration
	SweepCycles   int
	SweepMembers  int
	// MaxOutboxLag is how long the oldest undelivered terminal report may wait
	// before the controller reports itself unhealthy (default 15m).
	MaxOutboxLag time.Duration
}

// Deps are one epoch's dependencies.
type Deps struct {
	Handle *lease.Handle
	Fence  *lease.Manager
	// Pool is the controller pool, for reads (streaks, receipts, the watermark).
	Pool *pgxpool.Pool

	// Releaser releases a run's claims (claims.Plane); Claimed lists the item IDs
	// a run holds and Holders the runs that hold any claim.
	Releaser terminalawait.Releaser
	Claimed  func(runID string) ([]string, error)
	Holders  func(ctx context.Context) ([]string, error)

	// Temporal is the workflow client; Open lists the open runs and Closed the
	// runs closed after a time (Temporal visibility).
	Temporal terminalawait.Workflows
	Open     func(ctx context.Context) (map[string]bool, error)
	Closed   func(ctx context.Context, since time.Time) ([]terminalawait.ClosedRun, error)

	// Pins reads a run's start-fence pin; Bind resolves the repositories a run may
	// write to. Writer is the only path to the provider (terminalwriter.Writer).
	Pins   func(ctx context.Context, runID string) (terminalawait.RunPin, error)
	Bind   func(ctx context.Context, runID string) (terminalwriter.Binding, error)
	Writer providerwriter.Writer

	// Backends and Instance verify a recorded block's items through the applied
	// configuration, with no run involved: a run's pin is swept days after it
	// ends. Instance binds the gaggle whose backlog is the repository. Optional;
	// without them verification falls back to the run that recorded the block.
	Backends terminalwriter.Backends
	Instance func(repo providers.RepositoryRef) (terminalwriter.Binding, bool)
	// Escalator writes the escalation of a cycle the sweep resolves, outside any
	// run (terminalwriter.InstanceWriter). Required for deferral: without it a cycle
	// that cannot be verified is escalated at once, labelled, never deferred.
	Escalator parking.Poster
	// Reserve accounts a provider read in the shared quota ledger and reports
	// whether it may be made now (nil allows all).
	Reserve func(providers.ProviderKind) bool
	// Scrubber redacts provider errors the instance-level reads return (the
	// writer scrubs its own).
	Scrubber journal.Scrubber

	// Events reads a run's journal (the PostgreSQL journal); a missing journal is
	// (nil, nil).
	Events func(ctx context.Context, runID string) ([]journal.Event, error)
	// Log records hook outcomes. It must be the epoch's FENCED scheduler log:
	// outcomes are part of the durable record.
	Log frame.Recorder
	// RunURL is a run's portal link, for failure comments (optional).
	RunURL func(runID string) (string, error)
	Report func(error)

	Tuning Tuning
	Now    func() time.Time
}

func (d Deps) validate() error {
	switch {
	case d.Handle == nil || d.Fence == nil || d.Pool == nil:
		return errors.New("terminal composition requires the epoch's lease handle, fence and pool")
	case d.Releaser == nil || d.Claimed == nil || d.Holders == nil:
		return errors.New("terminal composition requires the claim plane")
	case d.Temporal == nil || d.Open == nil || d.Closed == nil:
		return errors.New("terminal composition requires Temporal run listing")
	case d.Pins == nil || d.Bind == nil || d.Writer == nil:
		return errors.New("terminal composition requires run pins, a run binding and the provider writer")
	case d.Events == nil || d.Log == nil:
		return errors.New("terminal composition requires the run journal and the fenced instance log")
	}
	return nil
}

// Composition is one epoch's terminal machinery.
type Composition struct {
	// Awaiters fires the terminal hooks (epoch.Awaiters).
	Awaiters *terminalawait.Awaiters
	// Delivery drains the terminal-report outbox and heals the blocked records.
	Delivery *Delivery
}

// New assembles the epoch's terminal machinery.
func New(deps Deps) (*Composition, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	report := deps.Report
	if report == nil {
		report = func(error) {}
	}
	deps.Report = report
	outbox := terminalreportpg.Outbox{
		Store: terminalreportpg.Store{Pool: deps.Pool, Fence: deps.Fence}, Handle: deps.Handle,
		// The outcome event is recorded best-effort: the fenced log reports a failed
		// append (it never writes after the fence moves) but does not return it, and
		// failing the delivery on it would re-send a report the provider already
		// has. A lost outcome event costs the log entry, never the report.
		Log: func(ev journal.Event) error { deps.Log.AppendBestEffort(ev); return nil },
	}
	blocks := blockedrecordspg.Store{Fence: deps.Fence, Closed: closedVia(deps), Reserve: deps.Reserve, Backoff: &blockedrecordspg.Backoff{Now: deps.Now}, Defer: deps.Escalator != nil}
	c := &composer{deps: deps, outbox: outbox, blocks: blocks}
	delivery := newDelivery(deps, outbox, blocks, c.escalateCycle)

	tune := deps.Tuning
	awaiters, err := terminalawait.New(deps.Handle, terminalawait.Config{
		Temporal:     deps.Temporal,
		Identify:     terminalawait.IdentifyFromPins(deps.Pins),
		Open:         deps.Open,
		ClaimHolders: deps.Holders,
		Closed:       deps.Closed,
		Watermark:    watermark{store: terminalwatermarkpg.Store{Pool: deps.Pool, Fence: deps.Fence}, instance: deps.Handle.InstanceID},
		Framed:       framed{terminalframedpg.Store{Pool: deps.Pool, Fence: deps.Fence}},
		Hooks:        c.hooks,
		Margin:       tune.Margin, LagBound: tune.LagBound, ClosedEvery: tune.ClosedEvery,
		MaxConcurrent: tune.MaxConcurrent, RetryBase: tune.RetryBase, RetryMax: tune.RetryMax,
		Now: deps.Now, Report: report,
	})
	if err != nil {
		return nil, err
	}
	return &Composition{Awaiters: awaiters, Delivery: delivery}, nil
}

// composer builds the per-run frame.
type composer struct {
	deps   Deps
	outbox terminalreportpg.Outbox
	blocks blockedrecordspg.Store
}

// bindTimeout bounds resolving a run's repositories when its frame is built.
const bindTimeout = 15 * time.Second

// hooks builds the frame for one run. The run's repositories come from its
// start-fence pin (the writer's binding). A run that cannot be bound still gets
// its claim release (Finalize): the provider hooks report why they could not run,
// and a binding that can never succeed (a run with no pin) is permanent.
func (c *composer) hooks(handle *lease.Handle, id terminalawait.Identity) *frame.Hooks {
	d := c.deps
	ctx, cancel := context.WithTimeout(context.Background(), bindTimeout)
	defer cancel()
	bind, err := d.Bind(ctx, id.RunID)
	if err != nil {
		return unbound(terminalawait.Hooks(d.Releaser, handle, id, d.Log, d.Claimed), fmt.Errorf("bind run %s: %w", id.RunID, err))
	}
	state := c.stateFor(bind)
	policyFor := func(runID string) *escalationnotify.Policy {
		return &escalationnotify.Policy{
			Poster: providerposter.Poster{Writer: d.Writer, RunID: runID}, State: state,
			Reports: c.outbox, Gaggle: id.Gaggle, Events: d.eventsFor,
		}
	}
	return terminalawait.Hooks(d.Releaser, handle, id, d.Log, d.Claimed,
		terminalawait.WithParking(d.Writer, state),
		terminalawait.WithFailed(policyFor),
		terminalawait.WithNotify(policyFor),
		terminalawait.WithPrepare(terminalawait.PrepareDeps{Writer: d.Writer, Events: d.eventsFor, Reports: c.outbox, Gaggle: id.Gaggle}),
	)
}

// unbound keeps Finalize and makes every provider hook report err.
func unbound(h *frame.Hooks, err error) *frame.Hooks {
	h.ExistingFix = func(context.Context, terminalhooks.ExistingFixOutcome) error { return err }
	h.Blocked = func(context.Context, terminalhooks.BlockedOutcome) error { return err }
	h.Failed = func(context.Context, terminalhooks.FailedOutcome) error { return err }
	h.Escalate = func(context.Context, frame.Outcome, string, string, string) error { return err }
	h.Prepare = func(context.Context, frame.Outcome) error { return err }
	h.Notify = func(context.Context, frame.Outcome) error { return err }
	return h
}

// stateFor is escalationnotify's State (and parking's) for one run.
func (c *composer) stateFor(bind terminalwriter.Binding) terminalstate.State {
	d := c.deps
	return terminalstate.State{
		State: blockedrecordspg.State{
			Store: c.blocks, Handle: d.Handle, Claimed: d.Claimed,
			Backlog: func(providers.RepositoryRef) providers.RepositoryRef { return bind.WorkItems },
		},
		Streaks:  failurestreakpg.Store{Pool: d.Pool, Fence: d.Fence},
		NoWork:   noworkstreakpg.Store{Fence: d.Fence},
		Journal:  d.eventsFor,
		CodeRepo: bind.Workspace,
		RunLink:  d.RunURL,
	}
}

func (d Deps) eventsFor(runID string) ([]journal.Event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bindTimeout)
	defer cancel()
	return d.Events(ctx, runID)
}

// closedVia verifies a work item is closed. It reads through the applied
// configuration's binding for the repository first, which needs no run, and only
// falls back to the run that recorded the block (whose pin may be swept). Only a
// definite non-open state counts; an error or an empty state leaves the record.
func closedVia(deps Deps) blockedrecordspg.ClosedFunc {
	return func(ctx context.Context, runID string, repo providers.RepositoryRef, itemID string) (bool, error) {
		item, err := readItem(ctx, deps, runID, repo, itemID)
		if err != nil {
			return false, err
		}
		return item.State != "" && !strings.EqualFold(item.State, "open"), nil
	}
}

func readItem(ctx context.Context, deps Deps, runID string, repo providers.RepositoryRef, itemID string) (providers.WorkItem, error) {
	if deps.Instance != nil && deps.Backends != nil {
		if bound, ok := deps.Instance(repo); ok {
			backend, err := deps.Backends.For(ctx, bound, repo, string(capability.GitHubIssuesRead))
			if err != nil {
				return providers.WorkItem{}, deps.scrub(err)
			}
			item, err := backend.GetWorkItem(ctx, repo, itemID)
			return item, deps.scrub(err)
		}
	}
	return deps.Writer.WorkItem(ctx, providerwriter.ItemRef{RunID: runID, Repo: repo, ItemID: itemID})
}

// scrub redacts an instance-level read's error; the run-bound writer scrubs its own.
// The failure's kind (rate limited, not found, refused, ...) is classified BEFORE
// the text is scrubbed and kept on the error, so the blocked-record store can still
// decide whether to hold or escalate.
func (d Deps) scrub(err error) error {
	if err == nil || d.Scrubber == nil {
		return err
	}
	class, permanent := blockedrecordspg.ClassifyVerification(err)
	return &blockedrecordspg.VerifyError{Class: class, Permanent: permanent, Message: string(d.Scrubber.Scrub([]byte(err.Error())))}
}

// watermark binds terminalwatermarkpg.Store to the epoch's instance.
type watermark struct {
	store    terminalwatermarkpg.Store
	instance string
}

func (w watermark) Load(ctx context.Context) (time.Time, bool, error) {
	return w.store.Load(ctx, w.instance)
}

func (w watermark) Advance(ctx context.Context, handle *lease.Handle, through time.Time) error {
	return w.store.Advance(ctx, handle, through)
}

// framed is terminalframedpg.Store as the awaiter's receipt store.
type framed struct{ terminalframedpg.Store }

var (
	_ terminalawait.Watermark = watermark{}
	_ terminalawait.Framed    = framed{}
)

// escalateCycle is the sweep's escalation: every member of the resolved cycle is
// parked needs-human with the cycle report, through the instance-level writer, with
// every comment keyed by the cycle's identity so a repeat (a crash before the marker
// is cleared) writes nothing new. The escalation is also journaled once.
func (c *composer) escalateCycle(ctx context.Context, cycle blockedcycle.Result, _ blockedrecordspg.Deferral) error {
	key := blockedrecordspg.CycleKey(cycle.Affected)
	err := parking.EscalateCycle(ctx, keyedPoster{inner: c.deps.Escalator, cycle: key}, cycle)
	if err == nil {
		c.deps.Log.AppendBestEffort(journal.Event{
			Type: journal.EventRunnerAnnotation, Reason: "blocked-record cycle escalated by the sweep",
			Runner: map[string]any{"kind": "blocked_cycle_escalated", "cycle": key, "members": len(cycle.Affected), "unverified": cycle.Unverified, "driver": string(journal.DriverEngine)},
		})
	}
	return err
}

// keyedPoster gives every cycle comment a key derived from the cycle and the
// comment, so the provider dedupes a repeated escalation.
type keyedPoster struct {
	inner parking.Poster
	cycle string
}

func (p keyedPoster) UpdateWorkItem(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	if req.Comment != "" && req.IdempotencyKey == "" {
		sum := sha256.Sum256([]byte(req.Comment))
		req.IdempotencyKey = p.cycle + "/" + req.ID + "/" + hex.EncodeToString(sum[:6])
	}
	return p.inner.UpdateWorkItem(ctx, req)
}
