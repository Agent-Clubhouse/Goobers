package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/livejournal"
)

// stageannotations.go is the INSTANCE-ANNOTATION seam (Goobers#3898), the
// third member of the family claimledger.go and the state store already
// belong to: one interface, a file backend for a type-1/type-2 instance and
// the daemon's own callers, a plane backend for a stage pod, and a fail-closed
// gap in between.
//
// The annotations in question are the scheduler's cross-run narration —
// "this backlog item was eligible but blocked", "this item completed with only
// blocked work left", "this item's failure streak degraded it". They have
// always been journal.EventRunnerAnnotation events in the INSTANCE log, and
// the claiming path wrote them by opening that file directly. A mode-3 stage
// pod has no such file: the daemon's volume is not mounted, and mounting it
// would give a workflow-authored stage write access to every run's scheduling
// record. That single os-level dependency is what kept backlog-query pinned to
// StageRequiresInstanceRoot.
//
// The plane backend sends the same event to the daemon over the run-scoped
// journal emit route, which the pod already presents a bearer for, as a
// livejournal.OpInstanceAnnotation. The daemon appends it to the instance log,
// stamping the run from the REQUEST rather than the payload, so a pod
// principal can only ever annotate for its own run.

// stageAnnotator records instance-scoped runner annotations. Append is
// synchronous and returns the delivery error; a caller that has always
// treated its annotation as best-effort keeps doing so at its own call site,
// where that decision is visible.
type stageAnnotator interface {
	Append(journal.Event) error
	Close() error
}

// openStageAnnotator is the seam, swappable in tests.
var openStageAnnotator = stageAnnotatorFor

// ErrAnnotationPlaneIncomplete is the fail-closed refusal for a stage whose
// environment names the journal plane without everything the emit route
// needs. Never a fall-through to a local instance log: in the posture that
// produces this error there is no local instance log worth writing to, and a
// silently dropped scheduler annotation is precisely the invisible failure
// #3898 exists to remove.
var ErrAnnotationPlaneIncomplete = errors.New("goobers: the journal plane is configured for annotations but incompletely; refusing to fall back to a local instance log")

// stageAnnotatorFor selects the backend from the stage's environment, exactly
// as claimsclient.Select and stateclient.Select do for their planes.
func stageAnnotatorFor(l instance.Layout) (stageAnnotator, error) {
	if !journalPlaneSelected() {
		return openFileAnnotator(l)
	}
	endpoint := strings.TrimSpace(os.Getenv(journalclient.EnvEndpoint))
	token := strings.TrimSpace(os.Getenv(journalclient.EnvToken))
	runID := strings.TrimSpace(os.Getenv(journalclient.EnvRunID))
	gaggle := strings.TrimSpace(os.Getenv(journalclient.EnvGaggle))
	switch {
	case token == "":
		return nil, fmt.Errorf("%w: %s is set but %s is empty", ErrAnnotationPlaneIncomplete, journalclient.EnvEndpoint, journalclient.EnvToken)
	case runID == "":
		return nil, fmt.Errorf("%w: %s is set but %s is empty, and the emit route contains every annotation to the caller's own run", ErrAnnotationPlaneIncomplete, journalclient.EnvEndpoint, journalclient.EnvRunID)
	case gaggle == "":
		return nil, fmt.Errorf("%w: %s is set but %s is empty, and the writer resolves a run's directory by gaggle", ErrAnnotationPlaneIncomplete, journalclient.EnvEndpoint, journalclient.EnvGaggle)
	}
	return &planeAnnotator{
		emitter: &livejournal.HTTPEmitter{BaseURL: endpoint, Token: token},
		runID:   runID,
		gaggle:  gaggle,
	}, nil
}

// journalPlaneSelected reports whether the stage's environment names the
// journal plane — the predicate a seam reads to skip instance-root side work
// (an instance-log open) the plane owns.
func journalPlaneSelected() bool {
	return strings.TrimSpace(os.Getenv(journalclient.EnvEndpoint)) != ""
}

// openFileAnnotator is the type-1/type-2 path, byte-for-byte what the
// claiming path did inline before this seam existed.
func openFileAnnotator(l instance.Layout) (stageAnnotator, error) {
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		return nil, fmt.Errorf("open instance log: %w", err)
	}
	return &fileAnnotator{log: log}, nil
}

// fileAnnotator writes straight to the instance log on this host.
type fileAnnotator struct{ log *journal.InstanceLog }

func (a *fileAnnotator) Append(ev journal.Event) error {
	if a == nil || a.log == nil {
		return errors.New("goobers: no instance log open for annotations")
	}
	return a.log.Append(ev)
}

func (a *fileAnnotator) Close() error {
	if a == nil || a.log == nil {
		return nil
	}
	return a.log.Close()
}

// planeAnnotator emits each annotation over the run-scoped journal plane.
//
// One op per call rather than a batch: these annotations are produced one
// decision at a time inside a claim loop, the emit route is idempotent by
// key, and batching would mean holding a scheduler's narration in memory
// across a section that can fail — turning "the annotation did not reach the
// daemon" into "several did not, and the stage cannot say which".
type planeAnnotator struct {
	emitter interface {
		Emit(context.Context, livejournal.EmitRequest) (livejournal.EmitResponse, error)
	}
	runID  string
	gaggle string
	// ordinal disambiguates two annotations that are byte-identical apart
	// from when they happened. See annotationKey.
	ordinal atomic.Uint64
}

func (a *planeAnnotator) Append(ev journal.Event) error {
	// The daemon restamps this from the request; setting it here keeps the
	// event the emitter sends identical to the one the file backend writes,
	// so the two backends are comparable in a test.
	ev.RunID = a.runID
	if ev.Type == "" {
		ev.Type = journal.EventRunnerAnnotation
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	ctx, cancel := context.WithTimeout(context.Background(), annotationEmitTimeout)
	defer cancel()
	_, err := a.emitter.Emit(ctx, livejournal.EmitRequest{
		RunID:  a.runID,
		Gaggle: a.gaggle,
		Ops: []livejournal.Op{{
			Kind:  livejournal.OpInstanceAnnotation,
			Key:   a.annotationKey(ev),
			Event: &ev,
			Time:  ev.Time,
		}},
	})
	if err != nil {
		return fmt.Errorf("emit instance annotation for run %s: %w", a.runID, err)
	}
	return nil
}

func (a *planeAnnotator) Close() error { return nil }

// annotationEmitTimeout bounds one annotation round trip. Short: an
// annotation is narration alongside a claim decision the stage has already
// made, and a stage must not stall a scheduling loop on a slow daemon.
const annotationEmitTimeout = 15 * time.Second

// annotationKey derives the op's idempotency key.
//
// Content-derived plus a per-process ordinal, and the ordinal is the load-
// bearing half. A retried EMIT of one annotation must dedup — hence deriving
// from the content — but two GENUINELY distinct annotations in one claim
// session can be byte-identical apart from time (the same item skipped as
// blocked on two passes), and a purely content-derived key would silently
// swallow the second. The ordinal is process-local, so a retried STAGE
// re-annotates; that is the same shape the file backend has always had, where
// every append is unconditional.
func (a *planeAnnotator) annotationKey(ev journal.Event) string {
	// hash.Hash.Write never returns an error, so the digest is built into a
	// buffer first rather than scattering ignored error returns.
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s\n%s\n%s\n%s\n%s\n%d\n", a.runID, ev.Type, ev.Stage, ev.Workflow, ev.Reason, a.ordinal.Add(1))
	for _, key := range sortedRunnerKeys(ev.Runner) {
		fmt.Fprintf(&buf, "%s=%v\n", key, ev.Runner[key])
	}
	sum := sha256.New()
	sum.Write(buf.Bytes())
	return "instance-annotation:" + hex.EncodeToString(sum.Sum(nil))[:32]
}

// sortedRunnerKeys orders a Runner map's keys so the derived key is stable
// across map iteration order.
func sortedRunnerKeys(runner map[string]any) []string {
	if len(runner) == 0 {
		return nil
	}
	keys := make([]string, 0, len(runner))
	for key := range runner {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
