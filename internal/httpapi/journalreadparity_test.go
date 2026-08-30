package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/readservice"
)

// journalreadparity_test.go is the load-bearing half of #3880's verification.
//
// The whole design rests on one claim: a converted CLI reader gets the SAME
// answer whether it read the run directory on disk or the daemon's scrubbed
// projection over the plane. If that claim is false anywhere, a stage silently
// decides differently in a pod than it does on a daemon host — the exact class
// of bug decision 005 was ruling about. So these tests build one real run
// journal and drive both backends across it, comparing what the readers act on.

const parityGaggle = "acme-web"

// seedParityRun writes a run journal that exercises every shape a converted
// reader consumes: a gate verdict artifact, a stage-finished event with scalar
// outputs and declared artifacts, a named artifact.recorded event, and a
// terminal state.
func seedParityRun(t *testing.T, root, runID string) instance.Layout {
	t.Helper()
	layout := instance.NewLayout(root)
	at := time.Now().UTC().Add(-time.Hour)
	run, err := journal.Create(layout.ForGaggle(parityGaggle).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation", Gaggle: parityGaggle,
	}, nil, journal.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	verdictRef, err := run.RecordStageArtifact("merge-review", 1, "", runID+":merge-review/verdict.json",
		[]byte(`{"decision":"approve","summary":"looks good","findings":[]}`))
	if err != nil {
		t.Fatalf("record verdict artifact: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "merge-review", Stage: "merge-review",
		Attempt: 1, Verdict: "pass", Ref: &verdictRef,
	}); err != nil {
		t.Fatalf("append gate.evaluated: %v", err)
	}

	findingRef, err := run.RecordStageArtifact("analyze", 1, "", "finding.md",
		[]byte("---\nkind: gate-noise\nsubject: local-ci-gate\n---\n\nbody\n"))
	if err != nil {
		t.Fatalf("record finding artifact: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "analyze", Attempt: 1, Status: "success",
		Artifacts: []journal.Ref{findingRef},
		Outputs: map[string]any{
			"gateEdit": "removed",
			"subject":  "local-ci-gate",
			"count":    3,
			"ok":       true,
			// A non-scalar output: the projection drops it, so both backends
			// must agree that a reader cannot see it.
			"nested": map[string]any{"deep": "value"},
		},
	}); err != nil {
		t.Fatalf("append stage.finished: %v", err)
	}

	if _, err := run.RecordStageArtifact("gather-pr-context", 1, "", runID+":gather-pr-context/result",
		[]byte(`{"schemaVersion":"v1"}`)); err != nil {
		t.Fatalf("record context artifact: %v", err)
	}
	if err := run.Append(journal.Event{
		Type: journal.EventRunFinished, Status: "success",
	}); err != nil {
		t.Fatalf("append run.finished: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close run: %v", err)
	}
	return layout
}

// parityBackends returns the two readers over one seeded run: the on-disk one
// and one talking to a real daemon handler over HTTP with a pod principal.
func parityBackends(t *testing.T, runID string) (journalclient.Reader, journalclient.Reader) {
	t.Helper()
	root := t.TempDir()
	layout := seedParityRun(t, root, runID)

	offline, err := readservice.NewOfflineRuns(layout)
	if err != nil {
		t.Fatalf("read service: %v", err)
	}
	handler, err := NewHandler(&offlineBackedReader{
		fakeReader: &fakeReader{health: readservice.Health{Ready: true}},
		offline:    offline,
	}, RequireRoles(), discardLogger(),
		WithAuthenticator(&fakeAuthenticator{principal: &Principal{
			Subject: "run:" + runID, Issuer: PodPrincipalIssuer,
		}}))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	file, err := journalclient.OpenFile(layout, runID)
	if err != nil {
		t.Fatalf("open file backend: %v", err)
	}
	plane, err := journalclient.NewHTTP(journalclient.HTTPConfig{
		BaseURL: server.URL, Token: "pod-token", RunID: runID, Gaggle: parityGaggle,
	})
	if err != nil {
		t.Fatalf("open plane backend: %v", err)
	}
	return file, plane
}

// TestFileAndPlaneBackendsAgreeOnGateVerdict is apply-verdict's and
// elect-lander's read: find the last gate.evaluated for a gate, then read its
// artifact. The plane's projection omits journal-relative paths, so the ref
// the client rebuilds is digest-derived — this proves that rebuild addresses
// the same bytes.
func TestFileAndPlaneBackendsAgreeOnGateVerdict(t *testing.T) {
	file, plane := parityBackends(t, "parity-run")

	read := func(t *testing.T, reader journalclient.Reader) []byte {
		t.Helper()
		events, err := reader.Events()
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		var ref *journal.Ref
		for i := range events {
			event := &events[i]
			if event.Type == journal.EventGateEvaluated && event.Gate == "merge-review" && event.Ref != nil {
				ref = event.Ref
			}
		}
		if ref == nil {
			t.Fatal("no gate.evaluated event carried a verdict ref")
		}
		data, err := reader.ArtifactBytes(*ref)
		if err != nil {
			t.Fatalf("artifact bytes: %v", err)
		}
		return data
	}

	fromFile, fromPlane := read(t, file), read(t, plane)
	if string(fromFile) != string(fromPlane) {
		t.Fatalf("verdict bytes differ:\n file  = %s\n plane = %s", fromFile, fromPlane)
	}
	if len(fromFile) == 0 {
		t.Fatal("the seeded verdict artifact read as empty on both backends")
	}
}

// TestFileAndPlaneBackendsAgreeOnStageOutputsAndArtifacts covers
// gate-removal-guard's two reads: a stage's declared artifacts (finding.md)
// and its scalar outputs (gateEdit/subject).
func TestFileAndPlaneBackendsAgreeOnStageOutputsAndArtifacts(t *testing.T) {
	file, plane := parityBackends(t, "parity-run")

	type stageView struct {
		outputs   map[string]string
		artifacts [][]byte
	}
	read := func(t *testing.T, reader journalclient.Reader) stageView {
		t.Helper()
		events, err := reader.Events()
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		view := stageView{outputs: map[string]string{}}
		for i := range events {
			event := &events[i]
			if event.Type != journal.EventStageFinished || event.Stage != "analyze" {
				continue
			}
			for _, key := range []string{"gateEdit", "subject"} {
				if value, ok := event.Outputs[key].(string); ok {
					view.outputs[key] = value
				}
			}
			for _, ref := range event.Artifacts {
				data, err := reader.ArtifactBytes(ref)
				if err != nil {
					t.Fatalf("artifact bytes: %v", err)
				}
				view.artifacts = append(view.artifacts, data)
			}
		}
		return view
	}

	fromFile, fromPlane := read(t, file), read(t, plane)
	if !reflect.DeepEqual(fromFile.outputs, fromPlane.outputs) {
		t.Fatalf("stage outputs differ: file = %v, plane = %v", fromFile.outputs, fromPlane.outputs)
	}
	if fromFile.outputs["gateEdit"] != "removed" || fromFile.outputs["subject"] != "local-ci-gate" {
		t.Fatalf("outputs = %v, want the seeded classification", fromFile.outputs)
	}
	if len(fromFile.artifacts) != len(fromPlane.artifacts) {
		t.Fatalf("declared artifact count differs: file = %d, plane = %d",
			len(fromFile.artifacts), len(fromPlane.artifacts))
	}
	for i := range fromFile.artifacts {
		if string(fromFile.artifacts[i]) != string(fromPlane.artifacts[i]) {
			t.Fatalf("declared artifact %d differs:\n file  = %s\n plane = %s",
				i, fromFile.artifacts[i], fromPlane.artifacts[i])
		}
	}
	if len(fromFile.artifacts) == 0 {
		t.Fatal("the seeded stage declared an artifact but neither backend saw it")
	}
}

// TestFileAndPlaneBackendsAgreeOnNamedArtifactLookup is
// gather-ci-failures/gather-issue-context/respond-to-findings' read: an
// artifact.recorded event addressed by its NAME.
func TestFileAndPlaneBackendsAgreeOnNamedArtifactLookup(t *testing.T) {
	file, plane := parityBackends(t, "parity-run")

	read := func(t *testing.T, reader journalclient.Reader) []byte {
		t.Helper()
		events, err := reader.Events()
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		var ref *journal.Ref
		for i := range events {
			event := &events[i]
			if event.Type == journal.EventArtifactRecorded &&
				event.Name == "parity-run:gather-pr-context/result" && event.Ref != nil {
				ref = event.Ref
			}
		}
		if ref == nil {
			t.Fatal("no artifact.recorded event carried the named result")
		}
		data, err := reader.ArtifactBytes(*ref)
		if err != nil {
			t.Fatalf("artifact bytes: %v", err)
		}
		return data
	}

	if fromFile, fromPlane := read(t, file), read(t, plane); string(fromFile) != string(fromPlane) {
		t.Fatalf("named artifact differs:\n file  = %s\n plane = %s", fromFile, fromPlane)
	}
}

// TestFileAndPlaneBackendsAgreeOnEventEnvelopes compares the whole event
// stream on the envelope fields readers switch on, so a projection change that
// drops one of them fails here rather than in production.
func TestFileAndPlaneBackendsAgreeOnEventEnvelopes(t *testing.T) {
	file, plane := parityBackends(t, "parity-run")

	fileEvents, err := file.Events()
	if err != nil {
		t.Fatalf("file events: %v", err)
	}
	planeEvents, err := plane.Events()
	if err != nil {
		t.Fatalf("plane events: %v", err)
	}
	if len(fileEvents) != len(planeEvents) {
		t.Fatalf("event count differs: file = %d, plane = %d", len(fileEvents), len(planeEvents))
	}
	for i := range fileEvents {
		a, b := fileEvents[i], planeEvents[i]
		if a.Seq != b.Seq || a.Type != b.Type || a.Stage != b.Stage || a.Gate != b.Gate ||
			a.Name != b.Name || a.Status != b.Status || a.Attempt != b.Attempt ||
			!a.Time.Equal(b.Time) {
			t.Fatalf("event %d envelope differs:\n file  = %+v\n plane = %+v", i, a, b)
		}
		if (a.Ref == nil) != (b.Ref == nil) {
			t.Fatalf("event %d ref presence differs: file = %v, plane = %v", i, a.Ref, b.Ref)
		}
		if a.Ref != nil && a.Ref.Digest != b.Ref.Digest {
			t.Fatalf("event %d ref digest differs: file = %s, plane = %s", i, a.Ref.Digest, b.Ref.Digest)
		}
	}
}

// TestFileAndPlaneBackendsAgreeOnStageAttemptsAndPhase covers the two reads
// that are not event scans: the stage attempt list, and the run phase that
// backlog-query's failure streak counts.
func TestFileAndPlaneBackendsAgreeOnStageAttemptsAndPhase(t *testing.T) {
	file, plane := parityBackends(t, "parity-run")

	fileAttempts, err := file.StageAttempts("analyze")
	if err != nil {
		t.Fatalf("file stage attempts: %v", err)
	}
	planeAttempts, err := plane.StageAttempts("analyze")
	if err != nil {
		t.Fatalf("plane stage attempts: %v", err)
	}
	if len(fileAttempts) != len(planeAttempts) {
		t.Fatalf("attempt count differs: file = %d, plane = %d", len(fileAttempts), len(planeAttempts))
	}
	for i := range fileAttempts {
		if fileAttempts[i].Number != planeAttempts[i].Number ||
			fileAttempts[i].Visit != planeAttempts[i].Visit ||
			fileAttempts[i].Status != planeAttempts[i].Status ||
			fileAttempts[i].Class != planeAttempts[i].Class ||
			len(fileAttempts[i].Artifacts) != len(planeAttempts[i].Artifacts) {
			t.Fatalf("attempt %d differs:\n file  = %+v\n plane = %+v", i, fileAttempts[i], planeAttempts[i])
		}
	}

	filePhase, err := file.Phase()
	if err != nil {
		t.Fatalf("file phase: %v", err)
	}
	planePhase, err := plane.Phase()
	if err != nil {
		t.Fatalf("plane phase: %v", err)
	}
	if filePhase != planePhase {
		t.Fatalf("phase differs: file = %q, plane = %q", filePhase, planePhase)
	}
	if filePhase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want the seeded terminal phase", filePhase)
	}
}

// TestPlaneBackendVerifiesArtifactDigests proves the client is not trusting
// the daemon's bytes: a substituted body is rejected, not returned.
func TestPlaneBackendVerifiesArtifactDigests(t *testing.T) {
	root := t.TempDir()
	layout := seedParityRun(t, root, "parity-run")

	file, err := journalclient.OpenFile(layout, "parity-run")
	if err != nil {
		t.Fatalf("open file backend: %v", err)
	}
	events, err := file.Events()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var digest string
	for i := range events {
		if events[i].Ref != nil {
			digest = events[i].Ref.Digest
			break
		}
	}
	if digest == "" {
		t.Fatal("the seeded run recorded no artifact")
	}

	tampering := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not the recorded bytes"))
	}))
	t.Cleanup(tampering.Close)

	plane, err := journalclient.NewHTTP(journalclient.HTTPConfig{
		BaseURL: tampering.URL, Token: "pod-token", RunID: "parity-run", Gaggle: parityGaggle,
	})
	if err != nil {
		t.Fatalf("open plane backend: %v", err)
	}
	if _, err := plane.ArtifactByDigest(digest); err == nil {
		t.Fatal("a substituted artifact body was accepted; the client must verify digests")
	}
}

// TestFileBackendReportsMissingRunDistinctly pins the error the converted
// readers branch on: "this host has no journal for that run" must stay
// distinguishable from "the read failed", because one is benign and the other
// is a decision the stage must not make blind.
func TestFileBackendReportsMissingRunDistinctly(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if _, err := journalclient.OpenFile(layout, "no-such-run"); err == nil {
		t.Fatal("opening a nonexistent run succeeded")
	} else if !errors.Is(err, journalclient.ErrRunNotFound) {
		t.Fatalf("err = %v, want a journalclient.ErrRunNotFound", err)
	}
}

// TestFileCrossRunRunPhaseFindsSeededRun exercises the same-host cross-run
// reader terminalFailureStreak now depends on.
func TestFileCrossRunRunPhaseFindsSeededRun(t *testing.T) {
	root := t.TempDir()
	layout := seedParityRun(t, root, "parity-run")
	// The run directory must be where the walk expects it.
	if _, err := layout.FindRunDir("parity-run"); err != nil {
		t.Fatalf("seeded run is not discoverable: %v (looked under %s)",
			err, filepath.Join(layout.ForGaggle(parityGaggle).RunsDir()))
	}
	phase, err := journalclient.NewFileCrossRun(layout).RunPhase(context.Background(), "parity-run")
	if err != nil {
		t.Fatalf("run phase: %v", err)
	}
	if phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", phase)
	}
	if _, err := journalclient.NewFileCrossRun(layout).RunPhase(context.Background(), "no-such-run"); err == nil {
		t.Fatal("a missing run answered a phase; it must be an explicit error")
	}
}

// offlineBackedReader serves the three run-scoped read routes from a REAL
// journal-derived projection — the same readservice.Local code path the daemon
// runs — while leaving every other read stubbed. Parity is meaningless against
// a fake projection: the whole question is whether the daemon's real scrubbing
// preserves what the converted readers act on.
type offlineBackedReader struct {
	*fakeReader
	offline readservice.OfflineRuns
}

func (r *offlineBackedReader) RunEvents(ctx context.Context, runID string) (readservice.EventList, error) {
	return r.offline.RunEvents(ctx, runID)
}

func (r *offlineBackedReader) StageAttempts(ctx context.Context, runID, stage string) (readservice.AttemptList, error) {
	return r.offline.StageAttempts(ctx, runID, stage)
}

func (r *offlineBackedReader) Artifact(ctx context.Context, runID, digest string) (readservice.ArtifactContent, error) {
	return r.offline.Artifact(ctx, runID, digest)
}
