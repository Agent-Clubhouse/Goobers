//go:build cloudintegration

package childengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
	wf "github.com/goobers/goobers/internal/workflow"
)

const testInstance = "child-engine-instance"

// family is one PostgreSQL database with the lease-fenced store of its
// current controller, plus the shared blob custody every epoch reads.
type family struct {
	pool    *pgxpool.Pool
	blobs   blobstore.Store
	store   *acceptance.Store
	manager *cloudlease.Manager
	handle  *cloudlease.Handle
}

func newFamily(t *testing.T) *family {
	t.Helper()
	databaseURL := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set")
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	name := "childengine_" + strings.ToLower(strings.NewReplacer("/", "_", " ", "_", "-", "_").Replace(t.Name()))
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+identifier+" WITH (FORCE)")
		admin.Close()
	})
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	// Roles are cluster-wide: provision them here rather than rely on another
	// package having run first against the same server.
	pgtest.EnsureRoles(t, pool)
	if err := (migrate.Runner{Direct: pool, Catalog: catalog, BackupRef: "childengine-test", InstanceID: "migration", Holder: "migration"}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	blobs, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &family{pool: pool, blobs: blobs}
	f.takeOver(t, "controller-1")
	return f
}

// takeOver acquires the control lease as holder and binds a fresh store.
func (f *family) takeOver(t *testing.T, holder string) {
	t.Helper()
	store, err := acceptance.New(f.pool, testInstance)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := cloudlease.New(cloudlease.Config{Pool: f.pool, InstanceID: testInstance, Name: "control", Holder: holder})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	store.BindFence(manager, handle)
	store.BindChildBlobs(f.blobs)
	f.store, f.manager, f.handle = store, manager, handle
}

func (f *family) service(wait time.Duration) *Service {
	return &Service{Custody: f.store, Freeze: freezeForTest, Wait: wait, Actor: "control-plane", Now: time.Now}
}

// custodyOnly satisfies engine.ChildService so the engine's own activity
// wrappers, with their full receipt and manifest verification, drive the
// adapter. Parking and continuation are out of this adapter's scope.
type custodyOnly struct{ *Service }

func (custodyOnly) Park(context.Context, engine.ChildParkInput) error { return errors.New("unused") }
func (custodyOnly) Finalize(context.Context, engine.ChildFinalizeInput) (childresult.TerminalReturn, error) {
	return childresult.TerminalReturn{}, errors.New("unused")
}
func (custodyOnly) Complete(context.Context, engine.ChildCompletionInput) (apiv1.ContextPointer, error) {
	return apiv1.ContextPointer{}, errors.New("unused")
}

func activities(s *Service) *engine.Activities {
	return &engine.Activities{EngineChildren: custodyOnly{s}}
}

func freezeForTest(_ context.Context, a Accepted) (engine.FrozenChildManifest, error) {
	spec := apiv1.WorkflowSpec{Gaggle: a.Child.Identity.Gaggle, Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "work",
		Tasks: []apiv1.Task{{Name: "work", Type: apiv1.TaskDeterministic, Goal: "work", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: wf.TerminalComplete}}}
	preview := true
	in := engine.RunInput{RunID: a.Child.RunID, Gaggle: a.Child.Identity.Gaggle, WorkflowName: "generated", Version: 1, DSLVersion: "3.1", Spec: spec,
		PreviewFeaturesEnabled: &preview, LiveJournal: true, GooberDigest: journal.Digest([]byte("kit")), ConfigGeneration: journal.Digest(a.Payload)}
	digest, err := wf.ComputeDigest(wf.Definition{Name: in.WorkflowName, Version: in.Version, DSLVersion: in.DSLVersion, Spec: spec})
	if err != nil {
		return engine.FrozenChildManifest{}, err
	}
	in.WorkflowDigest = digest
	return engine.FrozenChildManifest{Version: 1, Source: a.Source, SourceDigest: journal.Digest(a.Source), CanonicalDigest: journal.Digest([]byte("canonical")), Input: in}, nil
}

// acceptChild plays the tool front end: AcceptChild mints the run and
// acceptance IDs the dispatch adapter's handoff request then carries.
func acceptChild(t *testing.T, f *family, parent, invocation string) engine.ChildAdmissionInput {
	t.Helper()
	occurrence := journal.StageAttemptID(parent, 0, "parent", 3)
	source := []byte("proposal " + invocation)
	identity := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "web", ParentRunID: parent}, StageOccurrence: occurrence, InvocationKey: invocation}
	child, _, err := f.store.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: identity, Actor: "parent-stage", Payload: []byte(`{"start":"` + invocation + `"}`),
		MaxChildren: 4, Proposal: &triggerqueue.ChildProposal{Digest: journal.Digest(source), Source: source}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	request := journal.ChildHandoffRequest{Gaggle: "web", ParentRunID: parent, RequestID: journal.Digest([]byte(child.RunID)), Action: "wait", ChildRunID: child.RunID,
		AcceptanceID: child.AcceptanceID, InvocationKey: invocation, SourceDigest: child.ProposalDigest, Origin: apiv1.ChildWorkflowOrigin{StageOccurrence: occurrence, AttemptID: occurrence}}
	return engine.ChildAdmissionInput{Request: request, ParentWorkflow: "parent-workflow", ParentWorkflowID: parent + "-wf", ParentTemporalRunID: "temporal-run-1", Stage: "parent", OriginSeq: 3}
}

func TestAdmitFreezesBindsAndLoadsThroughEngineContract(t *testing.T) {
	f := newFamily(t)
	in := acceptChild(t, f, "parent-a", "call-1")
	acts := activities(f.service(time.Hour))
	binding, err := acts.AdmitEngineChild(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if want := binding.Child.AcceptedAt.Add(time.Hour); !binding.Deadline.Equal(want) {
		t.Fatalf("deadline %v, want acceptance-pinned %v", binding.Deadline, want)
	}
	child, err := acts.LoadEngineChild(t.Context(), binding)
	if err != nil || child.RunID != in.Request.ChildRunID {
		t.Fatalf("load = %+v %v", child, err)
	}
	// A retry after a lost acknowledgement, even with a changed wait policy,
	// returns the identical receipt and never re-freezes or moves the deadline.
	retry, err := activities(f.service(2*time.Hour)).AdmitEngineChild(t.Context(), in)
	if err != nil || retry.ManifestDigest != binding.ManifestDigest || !retry.Deadline.Equal(binding.Deadline) {
		t.Fatalf("retry = %+v %v", retry, err)
	}
}

func TestAdmitRefusesForgedHandoffs(t *testing.T) {
	f := newFamily(t)
	in := acceptChild(t, f, "parent-b", "call-1")
	acts := activities(f.service(time.Hour))
	for name, change := range map[string]func(*engine.ChildAdmissionInput){
		"other acceptance": func(in *engine.ChildAdmissionInput) { in.Request.AcceptanceID = "trigger-forged" },
		"other source":     func(in *engine.ChildAdmissionInput) { in.Request.SourceDigest = journal.Digest([]byte("forged")) },
		"other tenant":     func(in *engine.ChildAdmissionInput) { in.Request.Gaggle = "api" },
		"unaccepted call":  func(in *engine.ChildAdmissionInput) { in.Request.InvocationKey = "never-accepted" },
	} {
		t.Run(name, func(t *testing.T) {
			forged := in
			change(&forged)
			if _, err := acts.AdmitEngineChild(t.Context(), forged); err == nil {
				t.Fatal("forged handoff admitted")
			}
		})
	}
}

func TestResetParentGetsExplicitRestartRefusal(t *testing.T) {
	f := newFamily(t)
	in := acceptChild(t, f, "parent-c", "call-1")
	acts := activities(f.service(time.Hour))
	binding, err := acts.AdmitEngineChild(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	reset := in
	reset.ParentTemporalRunID = "temporal-run-after-reset"
	if _, err := acts.AdmitEngineChild(t.Context(), reset); err == nil || !strings.Contains(err.Error(), "restarting an accepted generated child is unsupported") {
		t.Fatalf("reset parent = %v", err)
	}
	forged := binding
	forged.ParentTemporalRunID = reset.ParentTemporalRunID
	if _, err := acts.LoadEngineChild(t.Context(), forged); err == nil {
		t.Fatal("reset parent loaded the original owner's manifest")
	}
}

func TestCancelFencesFamilyAndRetainsCustody(t *testing.T) {
	f := newFamily(t)
	first := acceptChild(t, f, "parent-d", "call-1")
	acts := activities(f.service(time.Hour))
	binding, err := acts.AdmitEngineChild(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	if err := acts.CancelEngineChild(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	if err := acts.CancelEngineChild(t.Context(), binding); err != nil {
		t.Fatalf("cancel is not idempotent: %v", err)
	}
	if _, _, err := f.store.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: triggerqueue.ChildIdentity{ChildParent: binding.Child.Identity.ChildParent, StageOccurrence: "later", InvocationKey: "call-2"},
		Actor: "parent-stage", Payload: []byte(`{}`), MaxChildren: 4}, time.Now()); !errors.Is(err, triggerqueue.ErrParentCancelled) {
		t.Fatalf("fenced family accepted another child: %v", err)
	}
	if _, err := acts.LoadEngineChild(t.Context(), binding); err != nil {
		t.Fatalf("fence discarded retained manifest custody: %v", err)
	}
}

func TestAdmissionSurvivesRealLeaseTakeover(t *testing.T) {
	f := newFamily(t)
	in := acceptChild(t, f, "parent-e", "call-1")
	binding, err := activities(f.service(time.Hour)).AdmitEngineChild(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	predecessor, oldStore, oldHandle := f.handle.Epoch, f.store, f.handle
	pending := acceptChild(t, f, "parent-e2", "call-1") // accepted, not yet admitted
	if err := f.manager.Release(t.Context(), oldHandle); err != nil {
		t.Fatal(err)
	}
	f.takeOver(t, "controller-2")
	successor := activities(f.service(time.Hour))
	if _, err := successor.AdmitEngineChild(t.Context(), pending); err == nil {
		t.Fatal("successor wrote child custody before adopting it")
	}
	if _, err := f.store.AdoptChildren(t.Context(), predecessor); err != nil {
		t.Fatal(err)
	}
	// The predecessor's admission replays identically under the successor,
	// and its manifest still verifies end to end.
	retry, err := successor.AdmitEngineChild(t.Context(), in)
	if err != nil || retry.ManifestDigest != binding.ManifestDigest || retry.ParentTemporalRunID != binding.ParentTemporalRunID || !retry.Deadline.Equal(binding.Deadline) {
		t.Fatalf("successor retry = %+v %v", retry, err)
	}
	if _, err := successor.LoadEngineChild(t.Context(), retry); err != nil {
		t.Fatal(err)
	}
	if _, err := successor.AdmitEngineChild(t.Context(), pending); err != nil {
		t.Fatalf("successor could not admit adopted custody: %v", err)
	}
	// The predecessor's fence is gone: it can no longer accept or bind.
	identity := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "web", ParentRunID: "parent-e"}, StageOccurrence: "stale", InvocationKey: "call-3"}
	if _, _, err := oldStore.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: identity, Actor: "parent-stage", Payload: []byte(`{}`), MaxChildren: 4}, time.Now()); !errors.Is(err, cloudlease.ErrStale) {
		t.Fatalf("stale predecessor accepted a child: %v", err)
	}
}

func TestServiceRequiresConfiguration(t *testing.T) {
	for i, s := range []*Service{nil, {}, {Custody: (*acceptance.Store)(nil), Freeze: freezeForTest, Wait: 25 * time.Hour, Actor: "a", Now: time.Now}} {
		if _, err := s.Admit(context.Background(), engine.ChildAdmissionInput{}); err == nil {
			t.Fatal(fmt.Sprint("unconfigured service admitted: ", i))
		}
	}
}

// The ordinary start loop must never start a generated child: its Temporal
// parent owns the start. Pending and Uncertain skip every child acceptance,
// both before admission and after the engine start intent is bound.
func TestStartLoopNeverSeesEngineOwnedChildren(t *testing.T) {
	f := newFamily(t)
	ordinary, _, err := f.store.Accept(t.Context(), "manual/1", "operator", []byte(`{"request":1}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pendingIDs := func() []string {
		t.Helper()
		records, err := f.store.Pending(t.Context(), 100)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range records {
			ids = append(ids, r.ID)
		}
		return ids
	}
	in := acceptChild(t, f, "parent-f", "call-1")
	if got := pendingIDs(); fmt.Sprint(got) != fmt.Sprint([]string{ordinary.ID}) {
		t.Fatalf("pending before admission = %v, want only %s", got, ordinary.ID)
	}
	if _, err := activities(f.service(time.Hour)).AdmitEngineChild(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if got := pendingIDs(); fmt.Sprint(got) != fmt.Sprint([]string{ordinary.ID}) {
		t.Fatalf("pending after admission = %v, want only %s", got, ordinary.ID)
	}
	// A child acceptance left dispatching by a lost epoch is not ordinary
	// uncertain custody either; only the ordinary row is reconciled.
	if err := f.store.BeginDispatch(t.Context(), in.Request.AcceptanceID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.BeginDispatch(t.Context(), ordinary.ID); err != nil {
		t.Fatal(err)
	}
	uncertain, err := f.store.Uncertain(t.Context(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(uncertain) != 1 || uncertain[0].ID != ordinary.ID {
		t.Fatalf("uncertain = %+v, want only %s", uncertain, ordinary.ID)
	}
}
