// Package livejournal is the daemon's live journal service
// (distributed-state-and-coordination.md §8, DS4): the single writer that
// authors an engine run's journal from journal events emitted AS THEY HAPPEN,
// instead of the journal being an after-image projected from Temporal history
// at close. Sequence numbers are assigned here, at acceptance, in arrival
// order — emitters never propose them — and every op carries an idempotency
// key so a retried activity replaying its emissions is a no-op: the journal
// holds exactly one copy of each event, with the originally-allocated seq.
//
// The writer is not a second journaling scheme. It writes through
// internal/journal's own Run writer — journal.Create / journal.Recover, the
// per-run-dir lock, scrub-then-fsync appends, state.json checkpoints — so the
// resulting runs/<id>/ directory is byte-compatible with every existing
// consumer (readservice, the stalled-run sweep, conformance tooling), and the
// single-writer discipline is the journal package's existing one.
//
// Dedup state is the journal itself. Every applied op's event records its
// idempotency key under the non-normative Runner map (EmitKeyRunnerField), in
// the same fsynced append as the event — so the dedup record cannot tear away
// from the event it describes, and a daemon restart derives the applied-key
// set by reading the journal back (rehydrate). A sidecar dedup file was
// rejected because its write is a second, separately-crashable step: events
// appended but the sidecar lost would re-admit duplicates, which is exactly
// the failure idempotency keys exist to close. Runner.* is always excluded
// from conformance (§3.3), so the keys never touch the normative surface.
package livejournal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// Op kinds — the same vocabulary as the engine workflow's accumulated
// JournalOps (internal/engine/journal.go) and the history projection that
// replays them: a plain event append, a content-addressed artifact record,
// or an executor-produced span adopted by digest.
const (
	OpAppend   = "append"
	OpArtifact = "artifact"
	OpSpan     = "span"
)

// EmitKeyRunnerField is the Runner-map field an applied op's idempotency key
// is recorded under. Runner.* is the sanctioned non-normative namespace, so
// the key never enters the conformance view; its presence is also what marks
// a journal as live-authored (Authored) for the demoted repair projection.
const EmitKeyRunnerField = "emitKey"

// SpanUnavailableErrorCode marks the error event recorded in place of a span
// whose bytes could not be adopted — the same soft-failure code the history
// projection uses (internal/engine/projection.go spanUnavailableErrorCode),
// so live authorship and repair re-projection degrade identically.
const SpanUnavailableErrorCode = "span_unavailable"

// ErrTerminal reports an emit carrying a new (non-duplicate) op for a run
// whose journal is already terminal — run.finished has been applied (or a
// watchdog terminalized the run). Duplicates of already-applied ops still
// succeed; genuinely new work for a closed journal is refused, never
// appended after the terminal event.
var ErrTerminal = errors.New("livejournal: run journal is terminal")

// ErrUnknownRun reports an emit for a run the writer has never opened whose
// batch carries no Open header — nothing to create the journal from.
var ErrUnknownRun = errors.New("livejournal: run journal does not exist and the emit carries no open header")

// SpanSource fetches a recorded span's bytes by content digest (satisfied by
// internal/blobstore.Store). Optional: without one, span ops degrade to
// SpanUnavailableErrorCode error events exactly as the history projection
// does without a span source.
type SpanSource interface {
	Get(ctx context.Context, digest string) ([]byte, error)
}

// ArtifactOp is one content-addressed artifact record: bytes the workflow
// reconstructs deterministically (context manifests, gate verdicts).
type ArtifactOp struct {
	Stage     string               `json:"stage,omitempty"`
	Attempt   int                  `json:"attempt,omitempty"`
	Class     journal.AttemptClass `json:"class,omitempty"`
	Name      string               `json:"name"`
	Data      []byte               `json:"data"`
	Integrity apiv1.Integrity      `json:"integrity,omitempty"`
}

// SpanOp adopts one executor-recorded span (a harness transcript) by digest —
// the adopt-span-by-digest surface of the journal plane. The emitter never
// holds the bytes; the writer fetches them from its SpanSource and verifies
// the digest before committing.
type SpanOp struct {
	Stage      string               `json:"stage,omitempty"`
	Attempt    int                  `json:"attempt,omitempty"`
	Class      journal.AttemptClass `json:"class,omitempty"`
	Name       string               `json:"name"`
	DataSchema string               `json:"dataSchema,omitempty"`
	Ref        journal.Ref          `json:"ref"`
}

// Op is one journal write, in emission order. Key is the op's idempotency
// key, derived by the emitter from (runID, branch, scope, attempt, ordinal) —
// deterministic within an attempt, distinct across attempts (§8). Time is the
// timestamp the writer stamps the resulting event with verbatim (the
// projection's own clock-replay rule, #629): for an op built on the worker's
// in-process path (engine/emit.go's liveOpFrom) that is still the
// workflow-deterministic decision time, so a live-authored journal and a
// history re-projection of that path's events agree exactly. An op built by
// a mode-3 stage pod (podArtifactRecorder.Append, recordStageArtifacts) has
// no access to that deterministic time and instead stamps the pod's own wall
// clock (#3774) — real, but not guaranteed monotonic with the daemon's or
// with other pods': a pod whose clock runs behind can move a run's
// LastActivity backwards (journal.Run.append adopts Time unconditionally),
// and one running ahead can understate observedAt.Sub(LastActivityAt). No
// clamp or monotonic floor is applied; this is an accepted, currently
// untested risk, not a guarantee.
type Op struct {
	Kind     string         `json:"kind"`
	Key      string         `json:"key"`
	Event    *journal.Event `json:"event,omitempty"`
	Artifact *ArtifactOp    `json:"artifact,omitempty"`
	Span     *SpanOp        `json:"span,omitempty"`
	Time     time.Time      `json:"time"`
}

// OpenHeader carries what journal.Create needs the first time a run emits:
// the pinned identity and the immutable input snapshots. Sent on the first
// batch; ignored (beyond validation) when the journal already exists, so a
// retried open batch is idempotent.
type OpenHeader struct {
	Identity   journal.RunIdentity `json:"identity"`
	Item       *apiv1.BacklogItem  `json:"item,omitempty"`
	Graph      json.RawMessage     `json:"graph,omitempty"`
	Definition json.RawMessage     `json:"definition,omitempty"`
}

// EmitRequest is one batched emission for a run.
type EmitRequest struct {
	RunID  string      `json:"runId"`
	Gaggle string      `json:"gaggle"`
	Open   *OpenHeader `json:"open,omitempty"`
	Ops    []Op        `json:"ops"`
}

// EmitResponse reports what the writer did with a batch.
type EmitResponse struct {
	// Applied counts ops appended by this call.
	Applied int `json:"applied"`
	// Deduplicated counts ops whose idempotency key was already applied — a
	// retried activity replaying its emissions lands here.
	Deduplicated int `json:"deduplicated"`
	// Seq is the journal's highest durable sequence after the batch.
	Seq uint64 `json:"seq"`
	// Terminal reports that the run's journal is closed (run.finished applied).
	Terminal bool `json:"terminal,omitempty"`
}

// Option configures a Writer.
type Option func(*Writer)

// WithSpanSource lets the writer adopt span ops by digest.
func WithSpanSource(src SpanSource) Option {
	return func(w *Writer) { w.spans = src }
}

// WithObserver reports each durable append (journal.WithAppendObserver's
// shape) — the daemon wires the read-model intake here so SSE and the portal
// see a live run's events through the existing machinery.
func WithObserver(observer func(runID string, seq uint64)) Option {
	return func(w *Writer) { w.observer = observer }
}

// WithScrubber sets the boundary scrubber handed to the journal writer.
func WithScrubber(s journal.Scrubber) Option {
	return func(w *Writer) { w.scrubber = s }
}

// WithClock overrides the wall clock used for idle accounting (tests).
func WithClock(now func() time.Time) Option {
	return func(w *Writer) { w.now = now }
}

// Writer is the daemon live journal writer. One per daemon (DS1); safe for
// concurrent use. It holds each in-flight run's journal open between emits —
// the run-dir lock the journal writer takes is the existing single-writer
// discipline — and CloseIdle releases journals that have gone quiet so other
// owners (the stalled-run sweep, `goobers run abort`) can take the lock.
type Writer struct {
	runsDir  func(gaggle string) (string, bool)
	spans    SpanSource
	observer func(runID string, seq uint64)
	scrubber journal.Scrubber
	now      func() time.Time

	mu sync.Mutex
	// open holds the journals the writer currently has open for appending.
	open map[string]*liveRun
	// reserved holds runs an external repairer (the DS5 backfill) has taken
	// exclusive control of via Reserve; an Emit for such a run waits on the
	// channel instead of rehydrating a journal the repairer is about to
	// replace. Closed on release.
	reserved map[string]chan struct{}
}

type liveRun struct {
	mu           sync.Mutex
	gaggle       string
	dir          string
	jr           *journal.Run
	clock        *replayClock
	keys         map[string]uint64
	artifactRefs map[string]journal.Ref
	lastEmit     time.Time
}

// replayClock replays op timestamps into the journal writer, mirroring the
// projection's clock rule (#629) so event times are the workflow's
// deterministic decision times, not arrival jitter.
type replayClock struct {
	mu      sync.Mutex
	current time.Time
}

// set adopts t as the clock's current time — except a zero t, which is
// refused rather than adopted (#3774 defense in depth). Every legitimate
// emitter stamps a real Time on its Op (podArtifactRecorder.Append,
// recordStageArtifacts, engine/emit.go's liveOpFrom); a zero one reaching
// here means some caller forgot to. Adopting it would retroactively zero the
// clock this run's events are timestamped from — refusing it instead leaves
// the clock at its last known-good value, so the affected event is stamped
// with stale-but-real time rather than 0001-01-01T00:00:00Z.
//
// "Last known-good value" is a guarantee about this clock's own history, not
// about a freshly constructed one: a warm liveRun's clock only ever advances
// via applied ops, so refusing a zero one always leaves a real prior time in
// place. rehydrate builds a brand-new replayClock per reopen, so it is the
// one caller responsible for seeding that history from the journal itself
// (newestTimestamped(report.Events)) before this guard's promise holds
// there too — see rehydrate's own comments.
func (c *replayClock) set(t time.Time) {
	if t.IsZero() {
		return
	}
	c.mu.Lock()
	c.current = t
	c.mu.Unlock()
}

// newestTimestamped returns the time of the most recent event carrying a
// non-zero timestamp, or the zero time when none does. Mirrors
// internal/runner/stalled.go's helper of the same name and rule; duplicated
// rather than imported to keep livejournal's dependency on the runner
// package at zero.
func newestTimestamped(events []journal.Event) time.Time {
	for i := len(events) - 1; i >= 0; i-- {
		if !events[i].Time.IsZero() {
			return events[i].Time
		}
	}
	return time.Time{}
}

func (c *replayClock) nowFunc() func() time.Time {
	return func() time.Time {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.current
	}
}

// NewWriter constructs the live writer. runsDir resolves a gaggle name to its
// runs directory (the reconciler's own map, seam-shaped); unresolvable
// gaggles are refused at emit.
func NewWriter(runsDir func(gaggle string) (string, bool), opts ...Option) (*Writer, error) {
	if runsDir == nil {
		return nil, errors.New("livejournal: runs-directory resolver is required")
	}
	w := &Writer{runsDir: runsDir, now: time.Now, open: make(map[string]*liveRun), reserved: make(map[string]chan struct{})}
	for _, opt := range opts {
		opt(w)
	}
	return w, nil
}

// IsOpen reports whether the writer currently holds runID's journal open —
// the demoted repair projection must not touch such a run (a final emit may
// still be in flight even after Temporal reports the workflow closed).
//
// This is a point-in-time snapshot: an emit can rehydrate the run the
// instant after IsOpen returns false. A repairer that goes on to REPLACE the
// run directory must use Reserve instead, which holds the answer stable for
// the duration of the repair.
func (w *Writer) IsOpen(runID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.open[runID]
	return ok
}

// Reserve takes exclusive external control of runID for the duration of a
// repair (the DS5 crash-orphan backfill replacing the run directory). It
// refuses (ok=false) while the writer holds the run's journal open — the
// atomic form of the IsOpen skip: the check and the claim happen under one
// lock, so an emit cannot rehydrate between them — and while another
// reservation is already in flight.
//
// While reserved, an Emit for the run parks at acquire until release, then
// proceeds normally: it re-derives the journal's state under the run-dir
// lock (rehydrate), so it lands against whatever the repair published — a
// backfilled terminal journal refuses new ops with ErrTerminal — and never
// acknowledges an append into a directory the repair has unlinked.
//
// release must be called exactly once, on every path.
func (w *Writer) Reserve(runID string) (release func(), ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, open := w.open[runID]; open {
		return nil, false
	}
	if _, taken := w.reserved[runID]; taken {
		return nil, false
	}
	ch := make(chan struct{})
	w.reserved[runID] = ch
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			delete(w.reserved, runID)
			w.mu.Unlock()
			close(ch)
		})
	}, true
}

// Emit applies one batch: dedupes each op on its idempotency key, appends the
// rest in order with seq assigned at acceptance, and closes the journal when
// the terminal run.finished lands. The first batch for a run must carry the
// Open header (journal creation at first emit); a batch whose ops are all
// duplicates succeeds without reopening anything.
func (w *Writer) Emit(ctx context.Context, req EmitRequest) (EmitResponse, error) {
	if !apiv1.ValidRunID(req.RunID) {
		return EmitResponse{}, fmt.Errorf("livejournal: invalid run id %q", req.RunID)
	}
	if len(req.Ops) == 0 {
		return EmitResponse{}, errors.New("livejournal: emit carries no ops")
	}
	for i, op := range req.Ops {
		if op.Key == "" {
			return EmitResponse{}, fmt.Errorf("livejournal: op %d carries no idempotency key", i)
		}
	}
	run, err := w.acquire(req)
	if err != nil {
		return EmitResponse{}, err
	}
	run.mu.Lock()
	for run.jr == nil && !terminalKeyed(run) {
		// A concurrent CloseIdle released this journal between our acquire and
		// lock. It was forgotten from the map, so another acquire rehydrates a
		// fresh handle rather than returning this stale one.
		run.mu.Unlock()
		run, err = w.acquire(req)
		if err != nil {
			return EmitResponse{}, err
		}
		run.mu.Lock()
	}
	defer run.mu.Unlock()
	run.lastEmit = w.now()

	var resp EmitResponse
	for i := range req.Ops {
		applied, err := w.applyOp(ctx, run, req.Ops[i])
		if err != nil {
			w.finishRun(req.RunID, run, &resp)
			return resp, fmt.Errorf("livejournal: apply op %d for run %s: %w", i, req.RunID, err)
		}
		if applied {
			resp.Applied++
		} else {
			resp.Deduplicated++
		}
	}
	w.finishRun(req.RunID, run, &resp)
	return resp, nil
}

// finishRun records the batch outcome and, when the journal reached its
// terminal event, closes and forgets the run — the writer's map holds only
// live journals; everything else is rehydrated from disk on demand.
func (w *Writer) finishRun(runID string, run *liveRun, resp *EmitResponse) {
	if run.jr != nil {
		resp.Seq = run.jr.Seq()
		if terminalKeyed(run) {
			_ = run.jr.Close()
			run.jr = nil
			w.forget(runID)
			resp.Terminal = true
		}
		return
	}
	// Terminal journal answered purely from dedup state (no writer opened).
	resp.Terminal = true
	for _, seq := range run.keys {
		if seq > resp.Seq {
			resp.Seq = seq
		}
	}
	w.forget(runID)
}

func terminalKeyed(run *liveRun) bool { return run.terminalSeq() > 0 }

func (run *liveRun) terminalSeq() uint64 { return run.keys[terminalMarker] }

// terminalMarker is the internal dedup-map slot recording that run.finished
// was applied. It is not a valid emitter key (emitter keys are never empty
// segments only), so it cannot collide.
const terminalMarker = "\x00terminal"

// acquire finds the open run, rehydrates an existing journal, or creates one
// from the batch's Open header. A run under an external Reserve is waited
// out first: rehydrating mid-repair would open (and then lose) the very
// directory the repairer is about to replace.
func (w *Writer) acquire(req EmitRequest) (*liveRun, error) {
	w.mu.Lock()
	for {
		if run, ok := w.open[req.RunID]; ok {
			w.mu.Unlock()
			return run, nil
		}
		reservation, reserved := w.reserved[req.RunID]
		if !reserved {
			break
		}
		w.mu.Unlock()
		<-reservation
		w.mu.Lock()
	}
	defer w.mu.Unlock()
	runsDir, ok := w.runsDir(req.Gaggle)
	if !ok {
		return nil, fmt.Errorf("livejournal: gaggle %q has no configured runs directory", req.Gaggle)
	}
	dir := filepath.Join(runsDir, req.RunID)
	var run *liveRun
	var err error
	if journal.Recorded(dir) {
		run, err = w.rehydrate(req, dir)
	} else {
		run, err = w.create(req, runsDir, dir)
	}
	if err != nil {
		return nil, err
	}
	w.open[req.RunID] = run
	return run, nil
}

func (w *Writer) forget(runID string) {
	w.mu.Lock()
	delete(w.open, runID)
	w.mu.Unlock()
}

// create scaffolds the run journal at first emit. The batch's first op must
// be the run.started append — journal.Create appends it as part of creation,
// so the op is satisfied by the journal existing (applyOp's run.started
// arm), with the identity's StartedAt and the event time replayed from the
// op's deterministic timestamp.
func (w *Writer) create(req EmitRequest, runsDir, dir string) (*liveRun, error) {
	if req.Open == nil {
		return nil, fmt.Errorf("%w: run %s", ErrUnknownRun, req.RunID)
	}
	header := *req.Open
	if header.Identity.RunID != req.RunID {
		return nil, fmt.Errorf("livejournal: open header identity %q does not match run %q", header.Identity.RunID, req.RunID)
	}
	if header.Identity.Gaggle != req.Gaggle {
		return nil, fmt.Errorf("livejournal: open header gaggle %q does not match request gaggle %q", header.Identity.Gaggle, req.Gaggle)
	}
	if len(header.Graph) == 0 || len(header.Definition) == 0 {
		return nil, errors.New("livejournal: open header carries no pinned workflow graph/definition")
	}
	first := req.Ops[0]
	if first.Kind != OpAppend || first.Event == nil || first.Event.Type != journal.EventRunStarted {
		return nil, errors.New("livejournal: the opening batch must begin with run.started")
	}

	inputs := map[string][]byte{
		journal.PinnedWorkflowGraphInputName:      []byte(header.Graph),
		journal.PinnedWorkflowDefinitionInputName: []byte(header.Definition),
	}
	inputIntegrity := map[string]apiv1.Integrity{
		journal.PinnedWorkflowGraphInputName:      apiv1.IntegrityTrusted,
		journal.PinnedWorkflowDefinitionInputName: apiv1.IntegrityTrusted,
	}
	if header.Item != nil {
		item := *header.Item
		if !item.Integrity.Valid() {
			item.Integrity = apiv1.IntegrityUnapproved
		}
		b, err := json.Marshal(&item)
		if err != nil {
			return nil, fmt.Errorf("livejournal: marshal item snapshot: %w", err)
		}
		inputs["item"] = b
		inputIntegrity["item"] = item.Integrity
	}

	id := header.Identity
	id.StartedAt = first.Time

	clock := &replayClock{}
	clock.set(first.Time)
	opts := []journal.Option{journal.WithClock(clock.nowFunc()), journal.WithInputIntegrity(inputIntegrity)}
	if w.observer != nil {
		opts = append(opts, journal.WithAppendObserver(w.observer))
	}
	if w.scrubber != nil {
		opts = append(opts, journal.WithScrubber(w.scrubber))
	}
	jr, err := journal.Create(runsDir, id, inputs, opts...)
	if err != nil {
		return nil, fmt.Errorf("livejournal: create run journal for %s: %w", req.RunID, err)
	}
	return &liveRun{
		gaggle: req.Gaggle, dir: dir, jr: jr, clock: clock,
		keys:         map[string]uint64{},
		artifactRefs: map[string]journal.Ref{},
	}, nil
}

// rehydrate reattaches to an existing journal after a daemon restart or idle
// close: dedup state and the verdict-artifact refs are derived from the
// journal itself (the keys ride each event's Runner map), so no state beyond
// the journal has to have survived.
//
// The derivation comes from journal.Recover's OWN under-lock read
// (RecoverReport.Events) — one full parse, atomic with acquiring the writer.
// It must never come from a separate unlocked pre-read: Recover blocks on the
// run-dir lock, and CloseIdle exists precisely so other owners (the
// stalled-run sweep, `goobers run abort`) can take that lock and terminalize
// a quiet run — state derived before the lock misses their run.finished, and
// the writer would then silently append AFTER the terminal event. When the
// under-lock view shows a terminal journal, the writer is closed again
// without writing anything; the dedup view alone answers straggler re-emits,
// and a genuinely new op is refused with ErrTerminal in applyOp exactly as
// the doc promises.
//
// Reopen cost is one full O(N) parse (Recover's). If quiet-run reopen churn
// ever matters at much larger event counts, the bounded-tail option is to
// checkpoint the derived key/ref tables alongside state.json's LastSeq and
// parse only the tail past it — deliberately not done here (a second,
// separately-crashable dedup record is what the journal-as-dedup-state design
// rejected; a checkpoint would need the same lag-tolerant healing state.json
// gets).
func (w *Writer) rehydrate(req EmitRequest, dir string) (*liveRun, error) {
	clock := &replayClock{}
	// Seed the clock with the batch's first op time so anything Recover
	// itself appends (a torn-tail repaired event) carries a real,
	// workflow-adjacent timestamp instead of the zero time. This is only a
	// provisional seed: Recover has not read the journal's history yet, so
	// this cannot be the "last known-good value" replayClock.set's contract
	// promises. It is upgraded below once that history is in hand.
	clock.set(req.Ops[0].Time)
	opts := []journal.Option{journal.WithClock(clock.nowFunc())}
	if w.observer != nil {
		opts = append(opts, journal.WithAppendObserver(w.observer))
	}
	if w.scrubber != nil {
		opts = append(opts, journal.WithScrubber(w.scrubber))
	}
	jr, report, err := journal.Recover(dir, opts...)
	if err != nil {
		return nil, fmt.Errorf("livejournal: reopen journal for %s: %w", req.RunID, err)
	}
	run := &liveRun{
		gaggle:       req.Gaggle,
		dir:          dir,
		keys:         map[string]uint64{},
		artifactRefs: map[string]journal.Ref{},
	}
	terminal := false
	for _, ev := range report.Events {
		if key, ok := ev.Runner[EmitKeyRunnerField].(string); ok && key != "" {
			run.keys[key] = ev.Seq
		}
		if ev.Type == journal.EventArtifactRecorded && ev.Name != "" && ev.Ref != nil {
			run.artifactRefs[ev.Name] = *ev.Ref
		}
		if ev.Type == journal.EventRunFinished {
			terminal = true
			run.keys[terminalMarker] = ev.Seq
		}
	}
	if terminal {
		// Terminal under the lock: release the writer immediately, having
		// written nothing. jr stays nil on the returned run — duplicates
		// dedupe against keys; a genuinely new op is refused with ErrTerminal
		// in applyOp.
		if err := jr.Close(); err != nil {
			return nil, fmt.Errorf("livejournal: release terminal journal for %s: %w", req.RunID, err)
		}
		return run, nil
	}
	// Upgrade the seed to the newest timestamped event this run's own history
	// (report.Events, which by now also includes any torn-tail repair event
	// Recover just appended) actually shows — the true "last known-good
	// value". Without this, a reopen whose first applied op is itself
	// unstamped (#3774: a long-silent agentic stage resuming after
	// liveJournalIdleClose, or any daemon restart) would leave the clock at
	// the provisional, possibly-zero seed above: applyOp's clock.set(op.Time)
	// refuses to adopt that zero op time, so nothing would ever move the
	// clock off it. Calling set again here is safe even when req.Ops[0].Time
	// was itself a real, newer time — applyOp re-adopts it via its own
	// clock.set(op.Time) once the op is actually applied, after rehydrate
	// returns.
	clock.set(newestTimestamped(report.Events))
	run.jr = jr
	run.clock = clock
	return run, nil
}

// applyOp applies one op under the run's lock. Returns whether the op was
// appended (false = deduplicated).
func (w *Writer) applyOp(ctx context.Context, run *liveRun, op Op) (bool, error) {
	if _, applied := run.keys[op.Key]; applied {
		return false, nil
	}
	if op.Kind == OpAppend && op.Event != nil && op.Event.Type == journal.EventRunStarted && run.jr != nil && run.jr.Seq() >= 1 {
		// journal.Create appended run.started as part of creation; the op is
		// already durable. Applies equally to a redelivered open batch.
		run.keys[op.Key] = 1
		return false, nil
	}
	if run.jr == nil || terminalKeyed(run) {
		return false, fmt.Errorf("%w (op key %s)", ErrTerminal, op.Key)
	}
	run.clock.set(op.Time)
	switch op.Kind {
	case OpAppend:
		if op.Event == nil {
			return false, errors.New("append op carries no event")
		}
		ev := *op.Event
		if ev.Type == journal.EventGateEvaluated && ev.Name != "" {
			ref, ok := run.artifactRefs[ev.Name]
			if !ok {
				return false, fmt.Errorf("gate.evaluated references unrecorded artifact %q", ev.Name)
			}
			ev.Ref = &ref
		}
		ev.Runner = withEmitKey(ev.Runner, op.Key)
		if err := run.jr.Append(ev); err != nil {
			return false, err
		}
		run.keys[op.Key] = run.jr.Seq()
		if ev.Type == journal.EventRunFinished {
			run.keys[terminalMarker] = run.jr.Seq()
		}
		return true, nil
	case OpArtifact:
		a := op.Artifact
		if a == nil {
			return false, errors.New("artifact op carries no payload")
		}
		integrity := a.Integrity
		if integrity == "" {
			integrity = apiv1.IntegrityDerived
		}
		meta := map[string]any{EmitKeyRunnerField: op.Key}
		var ref journal.Ref
		var err error
		if a.Stage != "" {
			ref, err = run.jr.RecordStageArtifactAnnotated(a.Stage, a.Attempt, a.Class, a.Name, a.Data, integrity, meta)
		} else {
			ref, err = run.jr.RecordArtifactAnnotated(a.Name, a.Data, integrity, meta)
		}
		if err != nil {
			return false, err
		}
		run.keys[op.Key] = run.jr.Seq()
		run.artifactRefs[a.Name] = ref
		return true, nil
	case OpSpan:
		s := op.Span
		if s == nil {
			return false, errors.New("span op carries no payload")
		}
		data, err := w.fetchSpan(ctx, s.Ref.Digest)
		if err != nil {
			// Availability is a property of this adoption attempt, not of the
			// run — degrade to a visible error event rather than failing the
			// emit or dropping the span silently (the projection's own rule,
			// internal/engine/projection.go adoptSpan).
			appendErr := run.jr.Append(journal.Event{
				Type: journal.EventError, Stage: s.Stage, Attempt: s.Attempt, AttemptClass: s.Class,
				Error: &journal.ErrorDetail{
					Code:    SpanUnavailableErrorCode,
					Message: fmt.Sprintf("span %q (%s): %v", s.Name, s.Ref.Digest, err),
				},
				Runner: map[string]any{EmitKeyRunnerField: op.Key},
			})
			if appendErr != nil {
				return false, appendErr
			}
			run.keys[op.Key] = run.jr.Seq()
			return true, nil
		}
		if _, err := run.jr.RecordSpanAnnotated(s.Stage, s.Name, s.DataSchema, data, map[string]any{EmitKeyRunnerField: op.Key}); err != nil {
			return false, err
		}
		run.keys[op.Key] = run.jr.Seq()
		return true, nil
	default:
		return false, fmt.Errorf("unknown op kind %q", op.Kind)
	}
}

// fetchSpan fetches digest and confirms the bytes hash to it — the span
// source is an external dependency, and wrong content must surface as an
// unavailable span, never a silently mismatched one (projection parity).
func (w *Writer) fetchSpan(ctx context.Context, digest string) ([]byte, error) {
	if w.spans == nil {
		return nil, errors.New("no span source configured")
	}
	if digest == "" {
		return nil, errors.New("span op has no digest")
	}
	data, err := w.spans.Get(ctx, digest)
	if err != nil {
		return nil, err
	}
	if got := journal.Digest(data); got != digest {
		return nil, fmt.Errorf("fetched bytes hash to %s, want %s", got, digest)
	}
	return data, nil
}

func withEmitKey(runner map[string]any, key string) map[string]any {
	merged := make(map[string]any, len(runner)+1)
	for k, v := range runner {
		merged[k] = v
	}
	merged[EmitKeyRunnerField] = key
	return merged
}

// CloseIdle closes (and forgets) every open journal whose last emit is older
// than olderThan, releasing the run-dir lock so the stalled-run sweep — whose
// silence threshold is necessarily longer — can terminalize a wedged run
// instead of timing out on the writer's lock. A later emit transparently
// rehydrates. Returns the run ids closed.
func (w *Writer) CloseIdle(olderThan time.Duration) []string {
	cutoff := w.now().Add(-olderThan)
	w.mu.Lock()
	candidates := make(map[string]*liveRun, len(w.open))
	for id, run := range w.open {
		candidates[id] = run
	}
	w.mu.Unlock()
	var closed []string
	for id, run := range candidates {
		run.mu.Lock()
		if run.lastEmit.Before(cutoff) && run.jr != nil {
			_ = run.jr.Close()
			run.jr = nil
			w.forget(id)
			closed = append(closed, id)
		}
		run.mu.Unlock()
	}
	return closed
}

// Close releases every open journal (daemon shutdown).
func (w *Writer) Close() {
	w.mu.Lock()
	open := w.open
	w.open = make(map[string]*liveRun)
	w.mu.Unlock()
	for _, run := range open {
		run.mu.Lock()
		if run.jr != nil {
			_ = run.jr.Close()
			run.jr = nil
		}
		run.mu.Unlock()
	}
}

// Authored reports whether events belong to a live-authored journal: any
// event carrying an emit key under Runner. A projection-written journal never
// carries one, which is what lets the demoted reconciler distinguish "verify
// this live record against history" from "project as before" (DS5).
func Authored(events []journal.Event) bool {
	for _, ev := range events {
		if key, ok := ev.Runner[EmitKeyRunnerField].(string); ok && key != "" {
			return true
		}
	}
	return false
}
