package engine

import (
	"reflect"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	wf "github.com/goobers/goobers/internal/workflow"
)

// reservationRunInput is one minimal engine run input: a single deterministic
// stage, so the compile succeeds and the identity is fully populated without
// the fixture asserting anything about stage content.
func reservationRunInput() RunInput {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "implement",
		Tasks: []apiv1.Task{{
			Name: "implement",
			Type: apiv1.TaskDeterministic,
			Goal: "implement",
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		}},
	}
	return RunInput{
		RunID:        "run-reserved",
		Gaggle:       "web",
		WorkflowName: "implementation",
		Version:      1,
		Spec:         spec,
		RepoRef:      apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item:         &apiv1.BacklogItem{ID: "42", Title: "an item"},
		TriggerKind:  "item",
		TriggerRef:   "issue:42",
		GooberDigest: "sha256:deadbeef",
		LiveJournal:  true,
	}
}

// TestReserveRunHeaderIsByteIdenticalToTheWorkflowsOwn is the hazard the
// reservation introduces and must not lose to.
//
// livejournal.Writer.applyOp ABSORBS a run.started append against a journal
// that already has one, and Emit's Open header is honored only when the
// journal is created. So whichever writer lands first authors run.yaml
// PERMANENTLY, and the other's header is silently discarded. If the
// reservation's header differed from the workflow's in any field, every run
// the reservation protects would carry a permanently wrong run identity —
// wrong workflow digest, wrong pinned definition, wrong gate capabilities —
// and nothing downstream would ever notice, because the workflow's own header
// never gets written.
//
// The assertion is therefore not "the reservation has a header" but "the
// reservation's header is the SAME VALUE the workflow's first emit would
// build", compared field by field against emitPending's own construction.
func TestReserveRunHeaderIsByteIdenticalToTheWorkflowsOwn(t *testing.T) {
	in := reservationRunInput()
	startedAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)

	req, err := ReserveRun(in, startedAt)
	if err != nil {
		t.Fatalf("ReserveRun: %v", err)
	}
	if req.Open == nil {
		t.Fatal("the reservation carries no Open header; without it the journal is not created and the run stays invisible to the boot scan")
	}

	// The workflow's own construction, from the same recorder emitPending
	// uses. Built here rather than asserted field-by-field against literals
	// so a NEW header field is covered the day it is added.
	normalized := in
	normalized.Item = normalizeItemIntegrity(normalized.Item)
	m, err := wf.Compile(
		wf.Definition{Name: in.WorkflowName, Version: in.Version, DSLVersion: in.DSLVersion, Spec: in.Spec},
		wf.WithPreviewFeatures(in.previewFeaturesEnabled()),
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	rec, err := newRunJournalRecorder(normalized, m)
	if err != nil {
		t.Fatalf("newRunJournalRecorder: %v", err)
	}
	want := &livejournal.OpenHeader{
		Identity:               rec.proj.Identity,
		Item:                   rec.proj.Item,
		Graph:                  rec.proj.Graph,
		Definition:             rec.proj.Definition,
		GateGooberCapabilities: rec.proj.GateGooberCapabilities,
	}
	if !reflect.DeepEqual(req.Open, want) {
		t.Fatalf("reservation header differs from the workflow's own:\n got %#v\nwant %#v", req.Open, want)
	}
	if req.RunID != in.RunID || req.Gaggle != in.Gaggle {
		t.Errorf("reservation addressed run %s/%s, want %s/%s", req.Gaggle, req.RunID, in.Gaggle, in.RunID)
	}
}

// TestReserveRunPinsTheGooberDigestIntoRunIdentity is piece 7's disk-side
// claim. The digest names the goober image the run's stages walk under; a run
// identity without it cannot answer "which goober produced this?" after the
// fact, and the worker kit-by-digest selection that consumes it (#3884)
// cannot be built on an identity that never carried it.
func TestReserveRunPinsTheGooberDigestIntoRunIdentity(t *testing.T) {
	in := reservationRunInput()
	in.InstanceID = "e62c1c105fdc4273a72d199394b41cb0"
	req, err := ReserveRun(in, time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("ReserveRun: %v", err)
	}
	if got := req.Open.Identity.GooberDigest; got != in.GooberDigest {
		t.Errorf("reserved run identity GooberDigest = %q, want %q", got, in.GooberDigest)
	}
	if req.Open.Identity.InstanceID != in.InstanceID {
		t.Fatalf("reservation lost pinned instance identity: %q", req.Open.Identity.InstanceID)
	}
}

// TestReserveRunEmitsExactlyOneRunStarted: the reservation's whole job is to
// create runs/<id>/ with a run.started, and no more. A reservation that
// emitted a second normative event would make the live journal's conformance
// view longer than the projection's, which DiffLiveJournal files as a
// live_journal_divergence on every protected run.
func TestReserveRunEmitsExactlyOneRunStarted(t *testing.T) {
	startedAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	req, err := ReserveRun(reservationRunInput(), startedAt)
	if err != nil {
		t.Fatalf("ReserveRun: %v", err)
	}
	var started int
	for _, op := range req.Ops {
		if op.Event != nil && op.Event.Type == journal.EventRunStarted {
			started++
			if !op.Event.Time.Equal(startedAt) && !op.Time.Equal(startedAt) {
				t.Errorf("run.started stamped %v / op %v, want the daemon's admission instant %v", op.Event.Time, op.Time, startedAt)
			}
		}
	}
	if started != 1 {
		t.Fatalf("reservation carries %d run.started events in %d ops, want exactly 1", started, len(req.Ops))
	}
	if len(req.Ops) != 1 {
		t.Fatalf("reservation carries %d ops, want exactly the one run.started; a second normative event diverges every protected run", len(req.Ops))
	}
	for _, op := range req.Ops {
		if op.Key == "" {
			t.Error("reservation op has no idempotency key; a retried reservation would double-append")
		}
	}
}

// TestReserveRunRefusesAnIncompleteReservation: the reservation is the run's
// permanent identity, so a reservation that could not be built must REFUSE
// the dispatch rather than proceed unreserved — proceeding would reopen the
// start-to-first-emit window the reservation exists to close, and no workflow
// has been started yet, so refusing costs one tick.
func TestReserveRunRefusesAnIncompleteReservation(t *testing.T) {
	noRunID := reservationRunInput()
	noRunID.RunID = ""
	if _, err := ReserveRun(noRunID, time.Now()); err == nil {
		t.Error("ReserveRun accepted an empty run id")
	}
	if _, err := ReserveRun(reservationRunInput(), time.Time{}); err == nil {
		t.Error("ReserveRun accepted a zero admission instant; run.started would be stamped with the zero time")
	}
}
