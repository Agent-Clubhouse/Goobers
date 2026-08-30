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
	// OpInstanceAnnotation records a runner annotation in the DAEMON'S
	// INSTANCE LOG rather than the run journal (Goobers#3898).
	//
	// It is the one op whose destination is not the run's own journal, and it
	// exists because the scheduler's own narration — "this backlog item was
	// eligible but blocked", "this item's failure streak degraded it" — is
	// instance-scoped by nature: it is read by an operator asking what the
	// scheduler DID across runs, not by anyone replaying one run's history.
	// It has always been written to the instance log, and a mode-3 stage pod
	// has no legal path to that file: the daemon's volume is not mounted, and
	// mounting it would hand a workflow-authored stage write access to every
	// run's scheduling record.
	//
	// So the pod emits it here, over the same run-scoped journal plane it
	// already presents a bearer for, and the DAEMON writes the file. The run
	// containment is what makes that safe: the route admits only a principal
	// for {run}, and the writer stamps the event's RunID from the request
	// rather than trusting the caller's — a pod cannot annotate for a run it
	// is not.
	OpInstanceAnnotation = "instance-annotation"
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

// ErrNoInstanceLog is returned for an OpInstanceAnnotation on a writer built
// without WithInstanceLog. Fail closed, deliberately: the alternative is a
// scheduler annotation that a pod believes it delivered and that no operator
// will ever read.
var ErrNoInstanceLog = errors.New("livejournal: no instance log configured for instance annotations")

// ErrUnknownRun reports an emit for a run the writer has never opened whose
// batch carries no Open header — nothing to create the journal from.
var ErrUnknownRun = errors.New("livejournal: run journal does not exist and the emit carries no open header")

// ErrAdoptConflict reports an Adopt for a run this writer already holds: it
// opened the journal itself (an engine-driven run — two drivers for one run is
// the bug, not a case to reconcile), or an earlier adoption has not been
// released. Refused rather than replaced, because the two handles would be two
// writers on one events.jsonl, which is exactly what D7's single-writer
// property forbids.
var ErrAdoptConflict = errors.New("livejournal: run journal is already held by this writer")

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
// untested risk, not a guarantee. It does not arise for an ADOPTED run
// (Adopt): Time is not replayed there at all, because the loaned handle stamps
// every event from its owner's single clock.
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
	// GateGooberCapabilities is the reviewer-goober capability map pinned at
	// run start (#294), carried here for the same reason Graph and Definition
	// are: the closed-run projection writer's inputs never reach a live
	// journal. journal.ReplaceRun keeps a complete live journal in place, so
	// an input this header omits is an input the run NEVER gets — and the
	// daemon credential plane's gate branch is fail-closed on an absent pin
	// (409 gate_pin_missing for every agentic reviewer gate).
	GateGooberCapabilities json.RawMessage `json:"gateGooberCapabilities,omitempty"`
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

// InstanceAppender is the daemon's instance log, narrowed to the single
// method OpInstanceAnnotation needs (*journal.InstanceLog satisfies it).
//
// Declared as an interface rather than taking the concrete type so this
// package keeps depending on journal for its EVENT vocabulary only, and so a
// test can assert what was written without a scheduler directory.
type InstanceAppender interface {
	Append(journal.Event) error
}

// WithInstanceLog gives the writer the daemon's instance log, enabling
// OpInstanceAnnotation. Without it that op kind is REFUSED, not dropped: a
// scheduler annotation that vanished because the daemon was assembled without
// this option would be exactly the silent partial-plane failure #3898 is
// about, only moved from the pod to the daemon.
func WithInstanceLog(log InstanceAppender) Option {
	return func(w *Writer) { w.instanceLog = log }
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
//
// For a run some OTHER driver in this process already holds the journal open
// for — the daemon's runner, driving a trigger-started run — the writer takes
// the handle on loan instead of opening a second one; see Adopt. One handle,
// one lock, either way.
type Writer struct {
	runsDir  func(gaggle string) (string, bool)
	spans    SpanSource
	observer func(runID string, seq uint64)
	scrubber journal.Scrubber
	now      func() time.Time
	// instanceLog is the daemon's instance log, the destination of
	// OpInstanceAnnotation. Nil in every non-daemon assembly of this writer,
	// which makes that op kind refuse rather than silently no-op.
	instanceLog InstanceAppender

	mu sync.Mutex
	// open holds the journals the writer currently has open for appending,
	// including the ones it holds on loan through Adopt (marked adopted, and
	// never closed from here).
	open map[string]*liveRun
	// reserved holds runs an external repairer (the DS5 backfill) has taken
	// exclusive control of via Reserve; an Emit for such a run waits on the
	// channel instead of rehydrating a journal the repairer is about to
	// replace. Closed on release.
	reserved map[string]chan struct{}
}

type liveRun struct {
	mu     sync.Mutex
	gaggle string
	dir    string
	jr     *journal.Run
	// clock is nil for an adopted run: the handle belongs to another driver
	// and was constructed with that driver's clock, which this writer cannot
	// (and must not) reach into. See Adopt.
	clock *replayClock
	// adopted marks jr as another driver's handle, on loan for the duration of
	// an Adopt. The writer appends through it but NEVER closes it and never
	// releases its run-dir lock — CloseIdle, Close and finishRun's terminal arm
	// all skip it; only the loan's own release, or the owner having closed the
	// handle behind the writer's back, drops the registration.
	// Immutable after construction, so it is safe to read under the WRITER's mu
	// (Adopt's conflict check) as well as under this run's.
	adopted bool
	// loans counts the outstanding Adopts of this same handle — a run's
	// concurrent parallel branches each take one for their own pod attempt (see
	// Adopt). Guarded by the WRITER's mu, not this run's.
	loans        int
	keys         map[string]uint64
	artifactRefs map[string]journal.Ref
	lastEmit     time.Time
}

// terminal reports whether the run's journal has reached its terminal event.
// Caller holds run.mu.
//
// For a journal this writer OWNS, its own dedup state is the whole truth: it is
// the only appender, so the marker applyOp latches on run.finished is complete.
//
// For an ADOPTED one it is not, and the gap is load-bearing. The owner appends
// run.finished through its own handle — the stalled-run sweep and `goobers run
// abort` terminalize exactly that way, and finishRun's doc says so — without
// ever passing through applyOp, so the marker deriveDedupState latched at Adopt
// time is a one-shot snapshot that never refreshes. The authority for a loaned
// handle is therefore the HANDLE: journal.Run tracks the phase its own appends
// imply and reports it through Phase(). Without this, a pod emit still in
// flight when the runner terminalizes is appended AFTER run.finished — the
// exact corruption ErrTerminal exists to refuse, and one the writer refuses
// correctly on every journal it opened itself.
//
// The residual window is one both appenders share and neither can close from
// here: the owner can append run.finished between this check and the append it
// guards, because two writers on one handle serialize only inside
// journal.Run's own mutex, one append at a time. That is orders of magnitude
// narrower than a snapshot frozen at Adopt, and it is the same window every
// check-then-append on a shared handle has.
func (run *liveRun) terminal() bool {
	if run.keys[terminalMarker] > 0 {
		return true
	}
	return run.adopted && run.jr != nil && run.jr.Phase() != journal.PhaseRunning
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
// still be in flight even after Temporal reports the workflow closed). An
// adopted run counts as open: another driver holds the handle, which is an
// even stronger reason for a repairer to stay away.
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

// Adopt lends the writer an ALREADY-OPEN journal handle for runID — the
// daemon runner's own *journal.Run for a run it is driving — so that
// pod-plane emits for that run append through the runner's handle instead of
// opening a second one. It returns a release that ends the loan; the handle
// itself stays the caller's to close.
//
// WHY (decision 003 ruling 5, first bullet). D7's single-writer property is
// enforced by a per-run-dir file lock: journal.Create and journal.Recover hold
// it for the lifetime of the *journal.Run they return (journal/run.go's
// acquireRunLock), and it is not reentrant — a second acquire through a
// separate descriptor blocks even inside one process, then fails with
// journal.ErrLockTimeout after journalLockTimeout, 30 seconds (journal/lock.go).
// For a runner-owned run the runner holds that lock for the whole run. Without
// this method a mode-3 stage pod's emit lands in Emit -> acquire -> rehydrate
// -> journal.Recover -> acquireRunLock, waits out the full 30s, and fails: the
// pod logs "record stage artifacts: ..." and surrenders pointers to bytes that
// were never stored, while the run's own journal shows nothing. Adoption keeps
// the invariant the lock exists to express — ONE handle, ONE lock — and makes
// the pod's emit an append on that one handle.
//
// Consequences the caller is buying, all of them properties of the adopted
// handle rather than of this writer:
//
//   - Event TIME is the adopted handle's clock, not the op's Time.
//     journal.Run.Append stamps every event from the clock its handle was
//     built with (journal/eventlog.go appendEvent), and the loaned handle's is
//     the runner's. So the writer does not replay Op.Time for an adopted run —
//     there is no replayClock to replay it into. That is the coherent choice
//     here, not a loss: an adopted journal's runner-written and pod-emitted
//     events then all come from ONE clock, which is precisely the cross-clock
//     hazard Op's own doc records (a pod running behind moving the run's
//     LastActivity backwards). The replay rule exists for engine-driven runs,
//     whose ops carry workflow-deterministic decision times a history
//     re-projection must reproduce; a runner-driven run has no such history.
//   - LastActivity ADVANCES on the handle the stalled-run watchdog holds
//     (journal.Run.append sets r.lastActivity, read by IfLastActivityBefore).
//     A pod stage emitting through an adopted handle is therefore visibly
//     alive to the sweep — the #3774 failure mode, where nothing advanced the
//     runner's handle because the pod wrote through the plane, does not arise.
//   - The runner's per-run SCRUBBER and append OBSERVER apply, since they are
//     the loaned handle's. Both are what the daemon wants: the observer is the
//     same read-model intake newLiveJournalWriter would have wired
//     (cmd/goobers/daemon.go's runIntakeObserver), so pod events reach SSE and
//     the portal mid-run, and the scrubber is the run's own credential-aware
//     one rather than this writer's default.
//
// Dedup state is derived here the same way rehydrate derives it — the applied
// emit keys ride the events themselves — but through journal.OpenReadOnly, a
// lock-free read: taking the lock is the very thing being avoided. A torn
// final record from an append in flight is skipped by Events(), not tripped
// over. The read can race a runner append it does not see, which is harmless:
// only this writer ever writes an EmitKeyRunnerField, so the keys a concurrent
// runner append could add are none.
//
// What is NOT a snapshot, and must not be, is the run's terminality: the owner
// appends run.finished through its own handle, so the writer reads that off the
// handle on every op rather than off the keys derived here (liveRun.terminal).
// The artifact refs a later gate.evaluated names ARE snapshotted, and refresh
// lazily from the journal on a miss (applyOp's gate.evaluated arm) for the same
// reason: the owner keeps recording artifacts this writer never saw.
//
// CONCURRENT LOANS. A run's parallel branches each dispatch their own pod
// attempt (internal/runner/parallel_run.go runs one goroutine per branch), so a
// per-attempt Adopt/release seam issues overlapping Adopts for one run. Those
// are not two writers — they are one handle, one lock, held once — so the loan
// is REFCOUNTED for the identical *journal.Run: the second Adopt joins the
// first, and the entry is dropped when the last release runs. A DIFFERENT
// handle for a run this writer already holds is still ErrAdoptConflict, refused
// rather than substituted, because that genuinely is two writers on one
// events.jsonl.
//
// Refusals are fail-closed: an id/gaggle whose runs directory does not resolve,
// a nil or already-CLOSED handle (a closed one can never accept an append, and
// keeping it would wedge every later emit behind a dead handle), a handle open
// on some other run's directory (which would land one run's pod bytes in
// another's journal), a run this writer already holds through a different
// handle (ErrAdoptConflict), and a run under an external Reserve, whose
// directory a repair is about to replace.
//
// EmitResponse.Seq for an adopted run is the handle's seq observed after the
// batch — a high-water mark of the whole journal, including events the owner
// appended concurrently — not necessarily the seq of the batch's own last
// event. The same is true of the seqs recorded in the dedup map, which are only
// ever tested for presence. Do not read either as "the seq of this op".
//
// release must be called exactly once per Adopt, on every path; it is
// idempotent and never touches jr. The LAST release ends the loan, and returns
// only once no emit is still inside the run — so the owner may close the handle
// the moment it returns. An emit arriving after that falls back to the ordinary
// rehydrate path, whose lock the owner's close has by then released.
func (w *Writer) Adopt(runID, gaggle string, jr *journal.Run) (release func(), err error) {
	if !apiv1.ValidRunID(runID) {
		return nil, fmt.Errorf("livejournal: invalid run id %q", runID)
	}
	if jr == nil {
		return nil, fmt.Errorf("livejournal: adopt run %s: no open journal handle", runID)
	}
	if jr.Closed() {
		return nil, fmt.Errorf("livejournal: adopt run %s: journal handle is closed", runID)
	}
	runsDir, ok := w.runsDir(gaggle)
	if !ok {
		return nil, fmt.Errorf("livejournal: gaggle %q has no configured runs directory", gaggle)
	}
	dir := filepath.Join(runsDir, runID)
	if got := filepath.Clean(jr.Dir()); got != filepath.Clean(dir) {
		return nil, fmt.Errorf("livejournal: adopt run %s: handle is open on %s, not %s", runID, got, dir)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return nil, fmt.Errorf("livejournal: adopt run %s: %w", runID, err)
	}
	events, err := reader.Events()
	if err != nil {
		return nil, fmt.Errorf("livejournal: adopt run %s: read applied keys: %w", runID, err)
	}
	fresh := &liveRun{
		gaggle: gaggle, dir: dir, jr: jr, adopted: true,
		keys:         map[string]uint64{},
		artifactRefs: map[string]journal.Ref{},
		lastEmit:     w.now(),
	}
	deriveDedupState(fresh, events)

	w.mu.Lock()
	run := fresh
	if existing, held := w.open[runID]; held {
		// adopted is immutable after construction, so reading it here is safe;
		// the short circuit is what keeps jr — which finishRun mutates under
		// the RUN's mu — out of reach for a run this writer owns.
		if !existing.adopted || existing.jr != jr {
			w.mu.Unlock()
			return nil, fmt.Errorf("%w: run %s", ErrAdoptConflict, runID)
		}
		// Same handle, already on loan: a concurrent branch's pod attempt.
		// Join the existing loan (and its live dedup state, which is fresher
		// than the derivation above) instead of refusing it.
		run = existing
	} else if _, reserved := w.reserved[runID]; reserved {
		w.mu.Unlock()
		return nil, fmt.Errorf("livejournal: adopt run %s: run is reserved for repair", runID)
	} else {
		w.open[runID] = fresh
	}
	run.loans++
	w.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			run.loans--
			last := run.loans <= 0
			if last {
				// Only this adoption's own entry: a re-adoption after release
				// must not be dropped by a stale release closure.
				if current, ok := w.open[runID]; ok && current == run {
					delete(w.open, runID)
				}
			}
			w.mu.Unlock()
			if !last {
				return
			}
			// Quiescence, and it is the caller's whole safety margin: a release
			// followed by jr.Close() must not race an emit that acquired this
			// run before the delete above and is inside it now. Taking the
			// run's own mutex waits that emit out. Nilling jr under it then
			// makes any emit still holding a stale reference re-acquire (Emit's
			// jr == nil loop) instead of appending through a handle the owner
			// is about to close.
			//
			// w.mu is released FIRST: an emit inside the run takes the writer's
			// mu (finishRun -> forget), so holding both here in the other order
			// is the deadlock this ordering avoids.
			run.mu.Lock()
			run.jr = nil
			run.mu.Unlock()
		})
	}, nil
}

// forgetLoan drops an adopted run's registration, but only while it is still
// the entry this loan installed — a re-adoption that replaced it keeps its own.
func (w *Writer) forgetLoan(runID string, run *liveRun) {
	w.mu.Lock()
	if current, ok := w.open[runID]; ok && current == run {
		delete(w.open, runID)
	}
	w.mu.Unlock()
}

// refreshAdopted re-derives an adopted run's cached view from the journal
// itself — the same lock-free read Adopt does, through the same derivation, so
// the refreshed view and the initial one cannot drift. Caller holds run.mu.
//
// A failed read is not an error here: it leaves the cached view exactly as it
// was, and the caller's own miss then produces the ordinary refusal.
func (w *Writer) refreshAdopted(run *liveRun) {
	reader, err := journal.OpenReadOnly(run.dir)
	if err != nil {
		return
	}
	events, err := reader.Events()
	if err != nil {
		return
	}
	deriveDedupState(run, events)
}

// deriveDedupState reads a journal's own events back into a run's dedup view:
// the applied idempotency keys, the artifact refs a later gate.evaluated
// references by name, and the terminal marker. Shared by rehydrate (a journal
// this writer owns) and Adopt (one it has on loan) so the two cannot drift.
func deriveDedupState(run *liveRun, events []journal.Event) (terminal bool) {
	for _, ev := range events {
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
	return terminal
}

// Emit applies one batch: dedupes each op on its idempotency key, appends the
// rest in order with seq assigned at acceptance, and closes the journal when
// the terminal run.finished lands. The first batch for a run must carry the
// Open header (journal creation at first emit); a batch whose ops are all
// duplicates succeeds without reopening anything.
//
// An ADOPTED run (see Adopt) takes this path unchanged except that it is found
// already open, so nothing rehydrates and no lock is taken.
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
	for run.jr == nil && !run.terminal() {
		// A concurrent CloseIdle released this journal between our acquire and
		// lock — or an adoption's release ended the loan, which nils jr for the
		// same reason. It was forgotten from the map, so another acquire
		// rehydrates a fresh handle rather than returning this stale one.
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
		applied, err := w.applyOp(ctx, req.RunID, run, req.Ops[i])
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
//
// An ADOPTED run is neither closed nor forgotten on the terminal count: the
// handle and its lock belong to the runner, and the adoption ends only at
// release. Terminality for such a run is read off the handle (liveRun.terminal),
// so a run the runner terminalized through its OWN handle refuses later ops
// with ErrTerminal too — not just one this plane happened to write
// run.finished for. resp.Seq is the handle's seq observed after the batch,
// which for an adopted run is a high-water mark of the whole journal (the owner
// appends to it concurrently), not necessarily the seq of this batch's last op.
//
// The one case that DOES drop an adopted run is a handle its owner closed
// without ending the loan. Nothing can be appended through it again, and its
// run-dir lock went with the close, so holding it would wedge every later emit
// behind a dead handle instead of letting the next one reopen the journal.
func (w *Writer) finishRun(runID string, run *liveRun, resp *EmitResponse) {
	if run.adopted {
		if run.jr != nil {
			resp.Seq = run.jr.Seq()
			resp.Terminal = run.terminal()
			if run.jr.Closed() {
				run.jr = nil
				w.forgetLoan(runID, run)
			}
			return
		}
		resp.Terminal = run.terminal()
		return
	}
	if run.jr != nil {
		resp.Seq = run.jr.Seq()
		if run.terminal() {
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
	// Mirrors engine/projection.go's writeProjectedRun exactly: the same
	// snapshot under the same name with the same integrity, so a live-authored
	// run.yaml and a re-projected one pin identical inputs.
	if len(header.GateGooberCapabilities) > 0 {
		inputs[journal.PinnedGateGooberCapabilitiesInputName] = []byte(header.GateGooberCapabilities)
		inputIntegrity[journal.PinnedGateGooberCapabilitiesInputName] = apiv1.IntegrityTrusted
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
	terminal := deriveDedupState(run, report.Events)
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
// appended (false = deduplicated). runID is the EMIT REQUEST's run — already
// checked against the caller's principal by the route — and is the identity
// an instance annotation is stamped with, never one the op carries.
func (w *Writer) applyOp(ctx context.Context, runID string, run *liveRun, op Op) (bool, error) {
	if _, applied := run.keys[op.Key]; applied {
		return false, nil
	}
	if op.Kind == OpAppend && op.Event != nil && op.Event.Type == journal.EventRunStarted && run.jr != nil && run.jr.Seq() >= 1 {
		// journal.Create appended run.started as part of creation; the op is
		// already durable. Applies equally to a redelivered open batch.
		run.keys[op.Key] = 1
		return false, nil
	}
	if run.jr == nil || run.terminal() {
		return false, fmt.Errorf("%w (op key %s)", ErrTerminal, op.Key)
	}
	if run.clock != nil {
		run.clock.set(op.Time)
	}
	// A nil clock is an ADOPTED run: the handle is another driver's and stamps
	// from its own clock, so there is nothing here to replay op.Time into. See
	// Adopt for why that is the coherent reading for a runner-driven run.
	switch op.Kind {
	case OpAppend:
		if op.Event == nil {
			return false, errors.New("append op carries no event")
		}
		ev := *op.Event
		if ev.Type == journal.EventGateEvaluated && ev.Name != "" {
			ref, ok := run.artifactRefs[ev.Name]
			if !ok && run.adopted {
				// The refs an adopted run starts with are a snapshot, and the
				// handle's OWNER keeps recording artifacts through it without
				// passing through this writer — a gate placed in a pod
				// (decision 001) naming a verdict the runner recorded is the
				// reachable shape. A name this writer never saw recorded is
				// therefore not evidence that no such artifact exists, so
				// re-derive from the journal before refusing. Only on a miss:
				// the ordinary path stays a map lookup.
				w.refreshAdopted(run)
				ref, ok = run.artifactRefs[ev.Name]
			}
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
	case OpInstanceAnnotation:
		if op.Event == nil {
			return false, errors.New("instance annotation op carries no event")
		}
		if w.instanceLog == nil {
			return false, ErrNoInstanceLog
		}
		ev := *op.Event
		// FOREIGN-RUN REFUSAL, writer half. The route already refuses a
		// principal whose subject is not this run, but the RunID the caller
		// put INSIDE the event is a second, independent channel: a pod
		// legitimately emitting for its own run could otherwise write an
		// instance-log entry attributed to any run it names, and the instance
		// log is the cross-run record an operator reads. Overwriting rather
		// than validating is deliberate — there is exactly one correct value
		// and the writer knows it, so there is no reason to make the caller
		// get it right.
		ev.RunID = runID
		if ev.Type == "" {
			ev.Type = journal.EventRunnerAnnotation
		}
		if ev.Time.IsZero() {
			ev.Time = op.Time
		}
		if ev.Time.IsZero() {
			ev.Time = w.now()
		}
		ev.Runner = withEmitKey(ev.Runner, op.Key)
		if err := w.instanceLog.Append(ev); err != nil {
			return false, err
		}
		// Marked applied at the run journal's CURRENT sequence: nothing was
		// appended to it, and the value is only ever compared for presence.
		// Dedup therefore holds for as long as the run stays open in memory —
		// deriveDedupState rebuilds from run-journal events, which these are
		// not, so a rehydrated run re-applies a replayed annotation. Accepted:
		// the instance log is an append-only narration, and a duplicated
		// scheduler annotation is noise, not a correctness failure.
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
//
// ADOPTED runs are never closed here. The handle is the runner's, the lock is
// already the runner's, and the run is not idle in any sense this sweep cares
// about — the runner is driving it. Closing a loaned handle would take the
// journal out from under its owner, and forgetting the adoption would send the
// next pod emit back down the rehydrate path Adopt exists to keep it off.
func (w *Writer) CloseIdle(olderThan time.Duration) []string {
	cutoff := w.now().Add(-olderThan)
	w.mu.Lock()
	candidates := make(map[string]*liveRun, len(w.open))
	for id, run := range w.open {
		if run.adopted {
			continue
		}
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

// Close releases every open journal the writer OWNS (daemon shutdown).
// Adopted handles are left alone and stay registered until their release: they
// are not this writer's to close, and their owner — the runner, shutting down
// on the same signal — closes them itself.
func (w *Writer) Close() {
	w.mu.Lock()
	owned := make([]*liveRun, 0, len(w.open))
	retained := make(map[string]*liveRun)
	for id, run := range w.open {
		if run.adopted {
			retained[id] = run
			continue
		}
		owned = append(owned, run)
	}
	w.open = retained
	w.mu.Unlock()
	for _, run := range owned {
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
