//go:build cloudintegration

package terminalwriter

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childworkflow"
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
func controllerJournal(t *testing.T) *journalpg.Backend {
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
	owner, err := pgxpool.NewWithConfig(t.Context(), config)
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
	controller := pgtest.ControllerPool(t, owner)
	var role string
	if err := controller.QueryRow(t.Context(), "SELECT current_user").Scan(&role); err != nil || role != "goobers_controller" {
		t.Fatalf("fixture must read as goobers_controller, got %q (%v)", role, err)
	}
	manager, err := lease.New(lease.Config{Pool: controller, InstanceID: "test-instance", Name: lease.ControllerName, Holder: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Release(context.Background(), handle) })
	backend, err := journalpg.New(journalpg.Config{Pool: controller, InstanceID: "test-instance", Lease: manager, LiveOwner: func() *lease.Handle { return handle }})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func TestJournalRunsBindsAsTheControllerRole(t *testing.T) {
	backend := controllerJournal(t)
	worked := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "fork"}
	for _, identity := range []journal.RunIdentity{
		{RunID: "run-engine", Gaggle: "core", Workflow: "implementation", ConfigGeneration: "gen-1"},
		{RunID: "run-workspace", Gaggle: "core", Workflow: "implementation", ConfigGeneration: "gen-1", WorkspaceRepository: &worked},
		{RunID: "run-unpinned", Gaggle: "core", Workflow: "implementation"},
		{RunID: "run-ado", Gaggle: "boards", Workflow: "implementation", ConfigGeneration: "gen-1"},
	} {
		if _, _, err := backend.CreateRun(t.Context(), journal.CreateRunRequest{Identity: identity}); err != nil {
			t.Fatal(err)
		}
	}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{
		ObjectMeta: metav1.ObjectMeta{Name: "core"},
		Spec: apiv1.GaggleSpec{
			Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "service"},
			Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "acme/service"},
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: "boards"},
		Spec: apiv1.GaggleSpec{
			Project: apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "org", Project: "Code", Name: "service"},
			Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "Backlog"},
		},
	}}}
	runs := JournalRuns{Journal: backend, Generations: pinnedGenerations{"gen-1": set}}

	bound, err := runs.Bind(t.Context(), "run-engine")
	if err != nil {
		t.Fatal(err)
	}
	// A same-provider GitHub backlog lives in the gaggle's project repository.
	service := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "service"}
	if !sameRepository(bound.Workspace, service) || !sameRepository(bound.WorkItems, service) || bound.Gaggle != "core" || bound.Generation != "gen-1" {
		t.Fatalf("engine run binding = %+v", bound)
	}
	bound, err = runs.Bind(t.Context(), "run-workspace")
	if err != nil {
		t.Fatal(err)
	}
	// Work items stay the gaggle's even when the run worked elsewhere.
	if !sameRepository(bound.Workspace, providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "fork"}) || !sameRepository(bound.WorkItems, service) {
		t.Fatalf("journaled workspace binding = %+v", bound)
	}
	// An ADO backlog is the code repository addressed in the backlog project.
	bound, err = runs.Bind(t.Context(), "run-ado")
	if err != nil {
		t.Fatal(err)
	}
	if !sameRepository(bound.Workspace, providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "Code", Name: "service"}) ||
		!sameRepository(bound.WorkItems, providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "Backlog", Name: "service"}) {
		t.Fatalf("ADO binding = %+v", bound)
	}
	for _, runID := range []string{"run-unpinned", "run-missing"} {
		if _, err := runs.Bind(t.Context(), runID); err == nil {
			t.Fatalf("%s bound without a pinned generation", runID)
		} else if runID == "run-unpinned" && !errors.Is(err, providerwriter.ErrRepositoryNotPinned) {
			t.Fatalf("%s: %v, want ErrRepositoryNotPinned", runID, err)
		}
	}
}
