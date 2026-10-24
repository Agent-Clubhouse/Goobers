//go:build cloudintegration

package terminalwriter

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/journalpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/migrate"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

type pinnedGenerations map[string]*instance.ConfigSet

func (g pinnedGenerations) Acquire(_ context.Context, generation string) (childworkflow.Generation, error) {
	set, ok := g[generation]
	if !ok {
		return childworkflow.Generation{}, errors.New("generation not retained")
	}
	return childworkflow.Generation{Set: set, Release: func() error { return nil }}, nil
}

// controllerJournal returns a journal backend whose every session is
// goobers_controller, the role the control plane's writer reads runs as.
func controllerJournal(t *testing.T) (*journalpg.Backend, *pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	backend, owner, controller, _, _ := controllerJournalLease(t)
	return backend, owner, controller
}

func controllerJournalLease(t *testing.T) (backend *journalpg.Backend, owner, controller *pgxpool.Pool, manager *lease.Manager, handle *lease.Handle) {
	t.Helper()
	url := os.Getenv("GOOBERS_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("GOOBERS_TEST_POSTGRES_URL is not set (local PG16 required)")
	}
	admin, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.EnsureRoles(t, admin)
	database := "terminalwriter_" + strings.ToLower(rand.Text())
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config := admin.Config()
	config.ConnConfig.Database = database
	config.MaxConns = lease.MaxFencedConnections
	owner, err = pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		owner.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)")
		admin.Close()
	})
	catalog, err := migrate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err := (migrate.Runner{Direct: owner, Catalog: catalog, BackupRef: "local-test", InstanceID: "migration", Holder: "test"}).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	controller = controllerPool(t, owner)
	var role string
	if err := controller.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != "goobers_controller" {
		t.Fatalf("fixture must read as goobers_controller, got %q (%v)", role, err)
	}
	manager, err = lease.New(lease.Config{Pool: controller, InstanceID: "test-instance", Name: lease.ControllerName, Holder: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err = manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Release(context.Background(), handle) })
	backend, err = journalpg.New(journalpg.Config{Pool: controller, InstanceID: "test-instance", Lease: manager, LiveOwner: func() *lease.Handle { return handle }})
	if err != nil {
		t.Fatal(err)
	}
	return backend, owner, controller, manager, handle
}

// TestJournalRunsBindsFromPinsAsTheControllerRole: the binding comes from the
// start fence's pins; the worker-written journal, read as goobers_controller,
// is only an agreement check, and a journal that disagrees refuses.
func TestJournalRunsBindsFromPinsAsTheControllerRole(t *testing.T) {
	backend, _, _ := controllerJournal(t)
	service := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "service"}
	fork := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "fork"}
	boards := apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "org", Project: "Code", Name: "service"}
	identity := func(runID, gaggle string, workspace *apiv1.RepoRef) journal.RunIdentity {
		return journal.RunIdentity{RunID: runID, Gaggle: gaggle, Workflow: "implementation", ConfigGeneration: "gen-1", WorkspaceRepository: workspace}
	}
	for _, id := range []journal.RunIdentity{
		identity("run-engine", "core", nil),
		identity("run-agreeing", "core", &service),
		identity("run-forked", "core", &fork),
		identity("run-forged-gaggle", "boards", nil),
		identity("run-unpinned", "core", nil),
		identity("run-ado", "boards", nil),
		identity("run-ghost", "ghost", nil),
		identity("run-elsewhere", "core", nil),
	} {
		if _, _, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{Identity: id}); err != nil {
			t.Fatal(err)
		}
	}
	pins := map[string]RunPin{
		"run-engine":        {Gaggle: "core", ConfigGeneration: "gen-1", RepoRef: service},
		"run-agreeing":      {Gaggle: "core", ConfigGeneration: "gen-1", RepoRef: service},
		"run-forked":        {Gaggle: "core", ConfigGeneration: "gen-1", RepoRef: service},
		"run-forged-gaggle": {Gaggle: "core", ConfigGeneration: "gen-1", RepoRef: service},
		"run-ado":           {Gaggle: "boards", ConfigGeneration: "gen-1", RepoRef: boards},
		"run-ghost":         {Gaggle: "ghost", ConfigGeneration: "gen-1", RepoRef: service},
		"run-elsewhere":     {Gaggle: "core", ConfigGeneration: "gen-1", RepoRef: fork},
		"run-no-journal":    {Gaggle: "core", ConfigGeneration: "gen-1", RepoRef: service},
	}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{
		ObjectMeta: metav1.ObjectMeta{Name: "core"},
		Spec:       apiv1.GaggleSpec{Project: service, Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "acme/service"}},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "boards"},
		Spec:       apiv1.GaggleSpec{Project: boards, Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "Backlog"}},
	}}}
	runs := JournalRuns{
		Pins: func(_ context.Context, runID string) (RunPin, error) {
			pin, ok := pins[runID]
			if !ok {
				return RunPin{}, sql.ErrNoRows
			}
			return pin, nil
		},
		Children: func(context.Context, string) (ChildLink, error) {
			return ChildLink{}, sql.ErrNoRows
		},
		Journal:     backend,
		Generations: pinnedGenerations{"gen-1": set},
	}

	serviceRef := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "service"}
	for _, runID := range []string{"run-engine", "run-agreeing"} {
		bound, err := runs.Bind(t.Context(), runID)
		if err != nil {
			t.Fatalf("%s: %v", runID, err)
		}
		// A same-provider GitHub backlog lives in the gaggle's project.
		if !sameRepository(bound.Workspace, serviceRef) || !sameRepository(bound.WorkItems, serviceRef) || bound.Gaggle != "core" || bound.Generation != "gen-1" {
			t.Fatalf("%s binding = %+v", runID, bound)
		}
	}
	// An ADO backlog is the code repository addressed in the backlog project.
	bound, err := runs.Bind(t.Context(), "run-ado")
	if err != nil {
		t.Fatal(err)
	}
	if !sameRepository(bound.Workspace, providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "Code", Name: "service"}) ||
		!sameRepository(bound.WorkItems, providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "Backlog", Name: "service"}) {
		t.Fatalf("ADO binding = %+v", bound)
	}
	for runID, why := range map[string]string{
		"run-forked":        "a journaled workspace that differs from the pin",
		"run-forged-gaggle": "a journaled gaggle that differs from the pin",
		"run-unpinned":      "a run with no start-fence pin",
		"run-no-journal":    "a pinned run with no journal",
		"run-ghost":         "a pinned gaggle with no project in the generation",
		"run-elsewhere":     "a pinned repository that is not the gaggle's project",
		"../escape":         "an invalid run ID",
	} {
		_, err := runs.Bind(t.Context(), runID)
		if !errors.Is(err, providerwriter.ErrRepositoryNotPinned) || !providerwriter.Permanent(err) {
			t.Errorf("%s (%s): %v, want a permanent ErrRepositoryNotPinned", runID, why, err)
		}
	}
	// A failed pin read is not a refusal: it is retried.
	runs.Pins = func(context.Context, string) (RunPin, error) { return RunPin{}, errors.New("connection reset") }
	if _, err := runs.Bind(t.Context(), "run-engine"); err == nil || providerwriter.Permanent(err) {
		t.Fatalf("pin read failure: %v, want a retryable error", err)
	}
}

// controllerPool opens a pool on base's database whose every session runs as
// goobers_controller, so the binding reads only what that role is granted.
func controllerPool(t *testing.T, base *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	config := base.Config().Copy()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE goobers_controller")
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestJournalRunsBindsFromTheStartFencesRunPin: composed as the control role
// will be, ordinary pins and child custody come through acceptance.Store,
// read as goobers_controller.
func TestJournalRunsBindsFromTheStartFencesRunPin(t *testing.T) {
	backend, owner, controller := controllerJournal(t)
	service := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "service"}
	if _, _, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{Identity: journal.RunIdentity{
		RunID: "run-pinned", Gaggle: "core", Workflow: "implementation", ConfigGeneration: "gen-1"}}); err != nil {
		t.Fatal(err)
	}
	repo, err := json.Marshal(service)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `
		INSERT INTO control.run_pin (instance_id, run_id, acceptance_id, gaggle, workflow, workflow_version, workflow_digest,
		  goober_digest, config_generation, repo_ref, branch_namespace, input_digest, owner_epoch)
		VALUES ('test-instance','run-pinned','acc-1','core','implementation',1,'wd','gd','gen-1',$1,'','id',1)`, repo); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	if _, _, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{Identity: journal.RunIdentity{
		RunID: "run-child", Gaggle: "core", Workflow: "implementation",
		ConfigGeneration: "gen-1", WorkflowDigest: digest, GooberDigest: digest,
		Child: &journal.ChildLineage{
			Gaggle: "core", ParentRunID: "run-pinned", ParentWorkflow: "implementation",
			StageOccurrence: "build/visit-1", InvocationKey: "first-child",
			AcceptanceID: "trigger-run-child", SourceDigest: digest, EnvelopeDigest: digest,
		},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `
		INSERT INTO control.acceptance (
			instance_id,id,idem_key,actor,fingerprint,payload,state,owner_epoch,run_id,accepted_at
		) VALUES (
			'test-instance','trigger-run-child','child-start','test',decode(repeat('aa',32),'hex'),
			'{}','dispatched',1,'run-child',clock_timestamp()
		);
		INSERT INTO control.child_parent (instance_id,gaggle,parent_run_id,owner_epoch)
		VALUES ('test-instance','core','run-pinned',1);
		INSERT INTO control.child_occurrence (
			instance_id,gaggle,parent_run_id,occurrence,accepted_count,max_children,owner_epoch
		) VALUES ('test-instance','core','run-pinned','build/visit-1',1,1,1);
		INSERT INTO control.child_lineage (
			instance_id,gaggle,parent_run_id,occurrence,invocation_key,sequence,child_id,
			acceptance_id,start_key,actor_digest,payload_digest,state,accepted_at,updated_at,owner_epoch
		) VALUES (
			'test-instance','core','run-pinned','build/visit-1','first-child',1,'child-run-child',
			'trigger-run-child','child-start',$1,$1,'running',clock_timestamp(),clock_timestamp(),1
		)`, digest); err != nil {
		t.Fatal(err)
	}
	store, err := acceptance.New(controller, "test-instance")
	if err != nil {
		t.Fatal(err)
	}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{
		ObjectMeta: metav1.ObjectMeta{Name: "core"},
		Spec:       apiv1.GaggleSpec{Project: service, Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "acme/service"}},
	}}}
	runs := JournalRuns{
		Pins: AcceptancePins(store.RunPinsOf), Children: AcceptanceChildren(store.ChildForExecutionRun),
		Journal: backend, Generations: pinnedGenerations{"gen-1": set},
	}
	bound, err := runs.Bind(t.Context(), "run-pinned")
	if err != nil {
		t.Fatalf("bind from the real run pin as goobers_controller: %v", err)
	}
	if !sameRepository(bound.Workspace, repositoryRef(service)) || bound.Gaggle != "core" || bound.Generation != "gen-1" {
		t.Fatalf("binding = %+v", bound)
	}
	childBound, err := runs.Bind(t.Context(), "run-child")
	if err != nil {
		t.Fatalf("bind child through parent pin as goobers_controller: %v", err)
	}
	if childBound.RunID != "run-child" || !sameRepository(childBound.Workspace, repositoryRef(service)) ||
		childBound.Gaggle != "core" || childBound.Generation != "gen-1" {
		t.Fatalf("child binding = %+v", childBound)
	}
	// A run the start fence never pinned has no row: a permanent refusal.
	if _, err := runs.Bind(t.Context(), "run-unpinned"); !errors.Is(err, providerwriter.ErrRepositoryNotPinned) || !providerwriter.Permanent(err) {
		t.Fatalf("unpinned run: %v, want a permanent ErrRepositoryNotPinned", err)
	}
}
