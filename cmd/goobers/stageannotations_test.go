package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// stageannotations_test.go is Goobers#3898's stage-side evidence.
//
// backlogClaimSession.journalBlockedSkips used to open the daemon's instance
// log directly, which is why a claiming backlog-query could never run in a
// pod: the daemon's volume is not mounted there, and mounting it would give a
// workflow-authored stage write access to every run's scheduling record.
// These tests pin the replacement seam: file backend for a type-1/type-2
// instance, run-scoped journal-emit plane for a pod, and a hard refusal in
// between rather than a silent local write nobody reads.

// stampJournalPlaneEnv is the environment the dispatcher stamps for the
// journal plane, so the production selection seam picks the plane backend.
func stampJournalPlaneEnv(t *testing.T, endpoint, token, runID, gaggle string) {
	t.Helper()
	t.Setenv(journalclient.EnvEndpoint, endpoint)
	t.Setenv(journalclient.EnvToken, token)
	t.Setenv(journalclient.EnvRunID, runID)
	t.Setenv(journalclient.EnvGaggle, gaggle)
}

// clearJournalPlaneEnv is the type-1/type-2 posture: no plane named at all.
func clearJournalPlaneEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{journalclient.EnvEndpoint, journalclient.EnvToken, journalclient.EnvRunID, journalclient.EnvGaggle} {
		t.Setenv(name, "")
	}
}

// With no plane named, the seam is byte-for-byte what the claiming path did
// before it existed: an append to this host's instance log.
func TestStageAnnotatorFallsBackToTheInstanceLogOffPlane(t *testing.T) {
	clearJournalPlaneEnv(t)
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	annotator, aerr := stageAnnotatorFor(layout)
	if aerr != nil {
		t.Fatalf("stageAnnotatorFor: %v", aerr)
	}
	if _, ok := annotator.(*fileAnnotator); !ok {
		t.Fatalf("annotator = %T, want the file backend for an off-plane instance", annotator)
	}
	if err := annotator.Append(journal.Event{
		Type:   journal.EventRunnerAnnotation,
		RunID:  "run-local",
		Reason: "blocked-eligibility-skip",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := annotator.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatalf("read instance log: %v", err)
	}
	if len(got) != 1 || got[0].Reason != "blocked-eligibility-skip" {
		t.Fatalf("instance log = %#v, want the one appended annotation", got)
	}
}

// FAIL CLOSED on a partial plane configuration, one arm per missing
// ingredient. Every one of these would otherwise degrade to a local instance
// log inside a pod: a file on a scratch volume, deleted with the pod, holding
// the scheduling narration an operator needs to explain why an item was never
// picked up. That is the exact invisible failure the issue is about, so the
// seam refuses rather than degrades.
func TestStageAnnotatorFailsClosedOnPartialPlaneConfig(t *testing.T) {
	for _, tc := range []struct {
		name        string
		endpoint    string
		token       string
		runID       string
		gaggle      string
		wantMissing string
	}{
		{"endpoint without a bearer", "http://daemon:7777", "", "run-1", "alpha", journalclient.EnvToken},
		{"endpoint without a run", "http://daemon:7777", "tok", "", "alpha", journalclient.EnvRunID},
		{"endpoint without a gaggle", "http://daemon:7777", "tok", "run-1", "", journalclient.EnvGaggle},
		{"whitespace bearer", "http://daemon:7777", "   ", "run-1", "alpha", journalclient.EnvToken},
		{"whitespace run", "http://daemon:7777", "tok", "  ", "alpha", journalclient.EnvRunID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stampJournalPlaneEnv(t, tc.endpoint, tc.token, tc.runID, tc.gaggle)
			// A pod's layout is a scratch directory. If the seam degraded to
			// the file backend it would create the scheduler directory here,
			// which the assertion below catches independently of the error.
			podLayout := instance.NewLayout(t.TempDir())
			annotator, err := stageAnnotatorFor(podLayout)
			if !errors.Is(err, ErrAnnotationPlaneIncomplete) {
				t.Fatalf("err = %v, want ErrAnnotationPlaneIncomplete", err)
			}
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("err = %v, want it to name the missing %s", err, tc.wantMissing)
			}
			if annotator != nil {
				t.Error("a usable annotator was returned alongside the refusal")
			}
			if _, statErr := os.Stat(podLayout.SchedulerDir()); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("the seam touched the pod's scratch scheduler directory (err = %v); it must not fall back", statErr)
			}
		})
	}
}

// The whole point, end to end: with nothing but the stamped environment, the
// production seam builds a plane annotator, and an append lands in the
// DAEMON's instance log — not in a file beside the pod.
func TestStampedStageEnvDeliversAnnotationsToTheDaemonsInstanceLog(t *testing.T) {
	daemon := newAnnotationPlane(t, "run-1", "alpha")
	stampJournalPlaneEnv(t, daemon.server.URL, daemon.token, "run-1", "alpha")

	podLayout := instance.NewLayout(t.TempDir())
	annotator, err := stageAnnotatorFor(podLayout)
	if err != nil {
		t.Fatalf("stageAnnotatorFor: %v", err)
	}
	if _, ok := annotator.(*planeAnnotator); !ok {
		t.Fatalf("annotator = %T, want the plane backend", annotator)
	}
	if err := annotator.Append(journal.Event{
		Type:   journal.EventRunnerAnnotation,
		Reason: "blocked-eligibility-skip",
		Runner: map[string]any{"item": "Agent-Clubhouse/Goobers#42"},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	got := daemon.log.snapshot()
	if len(got) != 1 {
		t.Fatalf("daemon instance log = %d events, want 1", len(got))
	}
	if got[0].Reason != "blocked-eligibility-skip" || got[0].Runner["item"] != "Agent-Clubhouse/Goobers#42" {
		t.Errorf("delivered event = %#v, want the stage's annotation intact", got[0])
	}
	if got[0].RunID != "run-1" {
		t.Errorf("run id = %q, want run-1", got[0].RunID)
	}
	if _, statErr := os.Stat(podLayout.SchedulerDir()); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a local scheduler directory was created beside the pod (err = %v)", statErr)
	}
}

// FOREIGN-RUN REFUSAL, route half. A pod holding run-1's journal bearer
// cannot annotate on behalf of run-2, whichever way it asks: by addressing
// run-2's emit route, or by naming run-2 inside the event it sends to its own.
func TestAnnotationPlaneRefusesAForeignRun(t *testing.T) {
	daemon := newAnnotationPlane(t, "run-1", "alpha")
	if _, err := journal.Create(daemon.layout.ForGaggle("alpha").RunsDir(),
		journal.RunIdentity{RunID: "run-2", Workflow: "implementation", Gaggle: "alpha"}, nil); err != nil {
		t.Fatal(err)
	}

	// (a) run-1's bearer, addressed at run-2's emit route.
	foreign := &planeAnnotator{
		emitter: &livejournal.HTTPEmitter{BaseURL: daemon.server.URL, Token: daemon.token},
		runID:   "run-2",
		gaggle:  "alpha",
	}
	if err := foreign.Append(journal.Event{Reason: "forged"}); err == nil {
		t.Fatal("run-1's bearer emitted an annotation for run-2")
	}
	if got := daemon.log.snapshot(); len(got) != 0 {
		t.Fatalf("the daemon's instance log gained %d events from a foreign-run emit", len(got))
	}

	// (b) run-1's own route, with run-2 named inside the payload. This one
	// SUCCEEDS as a request and is restamped: the writer overwrites RunID
	// from the request, so the forged attribution never reaches the log.
	own := &planeAnnotator{
		emitter: &livejournal.HTTPEmitter{BaseURL: daemon.server.URL, Token: daemon.token},
		runID:   "run-1",
		gaggle:  "alpha",
	}
	ev := journal.Event{Reason: "forged-attribution"}
	ev.RunID = "run-2"
	if err := own.Append(ev); err != nil {
		t.Fatalf("append on the pod's own run: %v", err)
	}
	got := daemon.log.snapshot()
	if len(got) != 1 {
		t.Fatalf("daemon instance log = %d events, want 1", len(got))
	}
	if got[0].RunID != "run-1" {
		t.Fatalf("instance log entry attributed to %q; the daemon must restamp from the request", got[0].RunID)
	}
}

// The annotator emits ONE op per Append, so a stage always knows exactly
// which annotation failed to deliver — the property the doc comment claims
// and the reason batching was rejected.
func TestPlaneAnnotatorEmitsOneOpPerAppend(t *testing.T) {
	recorder := &recordingEmitter{}
	annotator := &planeAnnotator{emitter: recorder, runID: "run-1", gaggle: "alpha"}
	for i := 0; i < 3; i++ {
		if err := annotator.Append(journal.Event{Reason: fmt.Sprintf("skip-%d", i)}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if len(recorder.requests) != 3 {
		t.Fatalf("emits = %d, want one per append", len(recorder.requests))
	}
	keys := map[string]bool{}
	for i, request := range recorder.requests {
		if len(request.Ops) != 1 {
			t.Fatalf("emit %d carried %d ops, want 1", i, len(request.Ops))
		}
		op := request.Ops[0]
		if op.Kind != livejournal.OpInstanceAnnotation {
			t.Errorf("emit %d kind = %q, want %q", i, op.Kind, livejournal.OpInstanceAnnotation)
		}
		if request.RunID != "run-1" || request.Gaggle != "alpha" {
			t.Errorf("emit %d addressed %s/%s, want alpha/run-1", i, request.Gaggle, request.RunID)
		}
		if op.Event == nil || op.Event.Type != journal.EventRunnerAnnotation {
			t.Errorf("emit %d event = %#v, want a defaulted runner annotation", i, op.Event)
		}
		if op.Event != nil && op.Event.Time.IsZero() {
			t.Errorf("emit %d carries no time", i)
		}
		keys[op.Key] = true
	}
	if len(keys) != 3 {
		t.Fatalf("emit keys = %v, want three distinct keys; two genuinely distinct annotations must not dedup into one", keys)
	}
}

// Idempotency without collision: a retried emit of the SAME op reuses its key
// (the daemon dedups), but two annotations produced by two separate Appends
// never share one, even when byte-identical apart from time — the same item
// skipped as blocked on two passes is two facts, and a purely
// content-derived key would swallow the second.
func TestAnnotationKeysAreContentDerivedAndOrdinalSeparated(t *testing.T) {
	annotator := &planeAnnotator{runID: "run-1", gaggle: "alpha"}
	ev := journal.Event{Type: journal.EventRunnerAnnotation, Reason: "blocked", Runner: map[string]any{"item": "#42"}}
	first := annotator.annotationKey(ev)
	second := annotator.annotationKey(ev)
	if first == second {
		t.Fatal("two distinct annotations with identical content produced the same key; the second would be silently swallowed")
	}
	if !strings.HasPrefix(first, "instance-annotation:") {
		t.Errorf("key = %q, want the instance-annotation namespace", first)
	}
	// Content participates: two annotators at the same ordinal but with
	// different content differ, so the key is not merely a counter.
	other := &planeAnnotator{runID: "run-1", gaggle: "alpha"}
	a := other.annotationKey(ev)
	yetAnother := &planeAnnotator{runID: "run-1", gaggle: "alpha"}
	b := yetAnother.annotationKey(journal.Event{Type: journal.EventRunnerAnnotation, Reason: "degraded"})
	if a == b {
		t.Error("annotations with different content share a key at the same ordinal")
	}
	// And the run id participates, so two pods never collide on the daemon.
	foreign := &planeAnnotator{runID: "run-2", gaggle: "alpha"}
	if foreign.annotationKey(ev) == a {
		t.Error("two runs' first annotations share a key")
	}
}

// Runner-map iteration order must not change a key: a map-ordered digest
// would make a retry look like a new annotation roughly half the time.
func TestAnnotationKeyIsStableAcrossMapOrder(t *testing.T) {
	ev := journal.Event{Type: journal.EventRunnerAnnotation, Reason: "blocked", Runner: map[string]any{
		"item": "#42", "blocked_by": "#41", "gaggle": "alpha", "workflow": "backlog-curation", "pass": 2,
	}}
	var want string
	for i := 0; i < 32; i++ {
		got := (&planeAnnotator{runID: "run-1", gaggle: "alpha"}).annotationKey(ev)
		if i == 0 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("key changed across map iteration order: %q then %q", want, got)
		}
	}
}

// --- the re-sweep state, the last file on the claiming path ---------------

// TestBacklogResweepStateWritesThroughToTheDaemonsFile is the migration's
// evidence. The re-sweep generation was a file the claiming path opened under
// the instance root directly; it is now a scheduler-state key, so a pod
// advances the DAEMON's generation under the daemon's own claims.lock instead
// of advancing a private copy on a scratch volume.
func TestBacklogResweepStateWritesThroughToTheDaemonsFile(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-1")
	stampStatePlaneEnv(t, plane, "goobers", token)

	podLayout := instance.NewLayout(t.TempDir())
	store, err := stageStateStore(podLayout)
	if err != nil {
		t.Fatal(err)
	}
	key := backlogResweepStateKey(providers.RepositoryRef{Owner: "Agent-Clubhouse", Name: "Goobers"}, "goobers", "trusted", "ready")

	state, err := readBacklogResweepState(t.Context(), store, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if state.Generation != 0 {
		t.Fatalf("generation = %d on a fresh key, want 0", state.Generation)
	}
	state.Cursor = "Agent-Clubhouse/Goobers#42"
	if err := advanceBacklogResweepState(t.Context(), store, key, 0, state); err != nil {
		t.Fatalf("advance: %v", err)
	}

	// It landed on the DAEMON, at the same bare filename the pre-migration
	// code used, so in-flight state survives the change.
	path := filepath.Join(plane.layout.SchedulerDir(), key)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the advance did not land on the daemon's scheduler directory: %v", err)
	}
	var persisted backlogResweepState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if persisted.Generation != 1 || persisted.Cursor != "Agent-Clubhouse/Goobers#42" {
		t.Fatalf("persisted = %+v, want generation 1 and the advanced cursor", persisted)
	}
	if _, statErr := os.Stat(filepath.Join(podLayout.SchedulerDir(), key)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a local copy was written beside the pod (err = %v); the plane write fell through", statErr)
	}
}

// The generation is a compare-and-swap value, and the CAS is what makes a
// pod-driven re-sweep and a daemon-driven one serialize instead of each
// advancing a private copy. A stale observed generation must lose.
func TestBacklogResweepStateAdvanceIsCompareAndSwap(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-1")
	key := backlogResweepStateKey(providers.RepositoryRef{Owner: "Agent-Clubhouse", Name: "Goobers"}, "goobers", "trusted", "ready")
	pod := plane.client(t, "goobers", token)
	daemon := plane.daemonStore(t)

	if err := advanceBacklogResweepState(t.Context(), daemon, key, 0, backlogResweepState{Cursor: "daemon"}); err != nil {
		t.Fatalf("daemon advance: %v", err)
	}
	// The pod still believes it observed generation 0. Its advance must be a
	// no-op, not an overwrite of the daemon's work.
	if err := advanceBacklogResweepState(t.Context(), pod, key, 0, backlogResweepState{Cursor: "pod"}); err != nil {
		t.Fatalf("pod advance: %v", err)
	}
	state, err := readBacklogResweepState(t.Context(), daemon, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if state.Generation != 1 || state.Cursor != "daemon" {
		t.Fatalf("state = %+v, want the daemon's advance to have survived a stale pod write", state)
	}
	// Now the pod observes the current generation and its advance lands.
	if err := advanceBacklogResweepState(t.Context(), pod, key, 1, backlogResweepState{Cursor: "pod"}); err != nil {
		t.Fatalf("pod advance at the current generation: %v", err)
	}
	state, err = readBacklogResweepState(t.Context(), daemon, key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if state.Generation != 2 || state.Cursor != "pod" {
		t.Fatalf("state = %+v, want the pod's advance at generation 2", state)
	}
}

// The key is a valid scheduler-state key for every input shape — an invalid
// one would be refused server-side, turning a re-sweep into a hard failure at
// the first advance rather than at review time.
func TestBacklogResweepStateKeyIsAlwaysAValidStateKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo providers.RepositoryRef
		rest [3]string
	}{
		{"ordinary", providers.RepositoryRef{Owner: "Agent-Clubhouse", Name: "Goobers"}, [3]string{"goobers", "trusted", "ready"}},
		{"empty gaggle", providers.RepositoryRef{Owner: "o", Name: "n"}, [3]string{"", "trusted", "ready"}},
		{"path-ish labels", providers.RepositoryRef{Owner: "../..", Name: "n/../.."}, [3]string{"../x", "a/b", "c\\d"}},
		{"all empty", providers.RepositoryRef{}, [3]string{"", "", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := backlogResweepStateKey(tc.repo, tc.rest[0], tc.rest[1], tc.rest[2])
			if !stateclient.ValidKey(key) {
				t.Fatalf("backlogResweepStateKey(...) = %q, which the plane refuses", key)
			}
		})
	}
	// Distinct shapes get distinct keys — one shared key would make two
	// unrelated re-sweeps fight over one generation counter.
	base := providers.RepositoryRef{Owner: "Agent-Clubhouse", Name: "Goobers"}
	seen := map[string]string{}
	for _, probe := range []struct {
		label string
		key   string
	}{
		{"base", backlogResweepStateKey(base, "goobers", "trusted", "ready")},
		{"other gaggle", backlogResweepStateKey(base, "other", "trusted", "ready")},
		{"other trust label", backlogResweepStateKey(base, "goobers", "other", "ready")},
		{"other ready label", backlogResweepStateKey(base, "goobers", "trusted", "other")},
		{"other repo", backlogResweepStateKey(providers.RepositoryRef{Owner: "o", Name: "n"}, "goobers", "trusted", "ready")},
	} {
		if prior, dup := seen[probe.key]; dup {
			t.Errorf("%s and %s share a re-sweep key", prior, probe.label)
		}
		seen[probe.key] = probe.label
	}
}

// --- harness --------------------------------------------------------------

// recordingEmitter captures emit requests without a server.
type recordingEmitter struct {
	requests []livejournal.EmitRequest
	err      error
}

func (e *recordingEmitter) Emit(_ context.Context, request livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	e.requests = append(e.requests, request)
	return livejournal.EmitResponse{}, e.err
}

// recordingInstanceLog captures the daemon's instance-log appends.
type recordingInstanceLog struct {
	mu     sync.Mutex
	events []journal.Event
}

func (l *recordingInstanceLog) Append(ev journal.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
	return nil
}

func (l *recordingInstanceLog) snapshot() []journal.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]journal.Event(nil), l.events...)
}

// annotationPlane is the daemon's REAL journal emit plane: podauth in front of
// a deny-all human fallback, httpapi.RequireRoles as the authorizer, and a
// livejournal.Writer carrying an instance log.
type annotationPlane struct {
	server *httptest.Server
	layout instance.Layout
	log    *recordingInstanceLog
	token  string
}

func newAnnotationPlane(t *testing.T, runID, gaggle string) *annotationPlane {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	instanceLog := &recordingInstanceLog{}
	writer, err := livejournal.NewWriter(func(g string) (string, bool) {
		if g != gaggle {
			return "", false
		}
		return layout.ForGaggle(g).RunsDir(), true
	}, livejournal.WithInstanceLog(instanceLog))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)

	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator),
		httpapi.WithJournalService(writer),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	// The run must exist on the daemon before an emit can address it: the
	// writer resolves a run's directory under its gaggle's runs root.
	run, err := journal.Create(layout.ForGaggle(gaggle).RunsDir(),
		journal.RunIdentity{RunID: runID, Workflow: "implementation", Gaggle: gaggle}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	token, err := registry.Mint(runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &annotationPlane{server: server, layout: layout, log: instanceLog, token: token}
}

// --- the call site the issue names ---------------------------------------

// TestBlockedSkipAnnotationsReachTheDaemonFromAPod is Goobers#3898 stated as
// the behaviour a reviewer can check: backlogClaimSession.journalBlockedSkips,
// running with nothing but a pod's stamped environment, delivers its
// blocked-eligibility narration into the DAEMON's instance log — the record an
// operator reads to answer "why has this item never been picked up?".
//
// Before this change that method wrote to a local *journal.InstanceLog, which
// in a pod is a scratch file deleted with the pod, so the answer to that
// question was permanently unavailable for any pod-executed claim cycle.
func TestBlockedSkipAnnotationsReachTheDaemonFromAPod(t *testing.T) {
	daemon := newAnnotationPlane(t, "run-1", "alpha")
	stampJournalPlaneEnv(t, daemon.server.URL, daemon.token, "run-1", "alpha")

	podLayout := instance.NewLayout(t.TempDir())
	annotator, err := openStageAnnotator(podLayout)
	if err != nil {
		t.Fatalf("openStageAnnotator: %v", err)
	}
	t.Cleanup(func() { _ = annotator.Close() })

	session := &backlogClaimSession{
		annotations: annotator,
		runID:       "run-1",
		workflow:    "backlog-curation",
		observedSkips: []blockedEligibilitySkip{
			{ItemID: "Agent-Clubhouse/Goobers#42", OpenBlockers: []string{"Agent-Clubhouse/Goobers#41"}},
			{ItemID: "Agent-Clubhouse/Goobers#43", ItemStateUnresolved: true},
			{ItemID: "Agent-Clubhouse/Goobers#44", VerificationPending: true, UnresolvedBlockers: []string{"#7"}},
		},
	}
	if err := session.journalBlockedSkips(); err != nil {
		t.Fatalf("journalBlockedSkips: %v", err)
	}

	got := daemon.log.snapshot()
	if len(got) != 3 {
		t.Fatalf("daemon instance log = %d events, want one per observed skip", len(got))
	}
	byItem := map[string]journal.Event{}
	for _, ev := range got {
		if ev.Type != journal.EventRunnerAnnotation {
			t.Errorf("event type = %q, want %q", ev.Type, journal.EventRunnerAnnotation)
		}
		if ev.RunID != "run-1" {
			t.Errorf("event run id = %q, want run-1", ev.RunID)
		}
		if ev.Runner["annotation"] != blockedEligibilitySkipAnnotation {
			t.Errorf("annotation marker = %v, want %q", ev.Runner["annotation"], blockedEligibilitySkipAnnotation)
		}
		item, _ := ev.Runner["itemId"].(string)
		byItem[item] = ev
	}
	for _, want := range []string{"Agent-Clubhouse/Goobers#42", "Agent-Clubhouse/Goobers#43", "Agent-Clubhouse/Goobers#44"} {
		ev, ok := byItem[want]
		if !ok {
			t.Fatalf("no annotation delivered for %s; delivered: %v", want, byItem)
		}
		if ev.Reason == "" {
			t.Errorf("%s: annotation carries no reason, so it explains nothing", want)
		}
		if ev.Workflow != "backlog-curation" {
			t.Errorf("%s: workflow = %q, want the session's", want, ev.Workflow)
		}
	}
	if byItem["Agent-Clubhouse/Goobers#43"].Runner["itemStateUnresolved"] != true {
		t.Error("the unresolved-item-state detail did not survive the plane")
	}
	if byItem["Agent-Clubhouse/Goobers#44"].Runner["verificationPending"] != true {
		t.Error("the verification-pending detail did not survive the plane")
	}

	// Nothing was written beside the pod.
	if _, statErr := os.Stat(podLayout.SchedulerDir()); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a local scheduler directory was created beside the pod (err = %v)", statErr)
	}
}

// A delivery failure is returned to the caller rather than swallowed: the
// claim cycle decides what an undelivered annotation means, and it can only
// decide if it is told.
func TestBlockedSkipAnnotationDeliveryFailuresSurface(t *testing.T) {
	session := &backlogClaimSession{
		annotations: &planeAnnotator{
			emitter: &recordingEmitter{err: errors.New("daemon unreachable")},
			runID:   "run-1", gaggle: "alpha",
		},
		runID:         "run-1",
		workflow:      "backlog-curation",
		observedSkips: []blockedEligibilitySkip{{ItemID: "Agent-Clubhouse/Goobers#42"}},
	}
	err := session.journalBlockedSkips()
	if err == nil {
		t.Fatal("an undeliverable annotation was reported as delivered")
	}
	if !strings.Contains(err.Error(), "Agent-Clubhouse/Goobers#42") {
		t.Errorf("err = %v, want it to name the item whose annotation was lost", err)
	}
}

// The claiming path holds no writable local instance log at all any more —
// the single os-level dependency that pinned `backlog-query --claim` to
// StageRequiresInstanceRoot. Asserted against the source so a future edit
// that reintroduces it fails here, where the reason is written down, rather
// than in a pod.
func TestTheClaimingPathOpensNoLocalInstanceLog(t *testing.T) {
	source, err := os.ReadFile("backlogquery.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(source), "journal.OpenInstanceLog") {
		t.Fatal("backlogquery.go opens an instance log directly again; annotations must go through openStageAnnotator, which a pod can serve")
	}
	resweep, err := os.ReadFile("backlogresweep.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"journal.OpenInstanceLog", "os.ReadFile", "os.WriteFile", "filepath.Join"} {
		if strings.Contains(string(resweep), forbidden) {
			t.Errorf("backlogresweep.go uses %s again; the re-sweep generation lives on the scheduler-state plane", forbidden)
		}
	}
}
