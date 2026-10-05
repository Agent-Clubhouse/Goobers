package main

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/worktree"
)

func TestPRRepairWorkflowReloadWaitsForEffectAndPublishesOneSnapshot(t *testing.T) {
	u, source, input := repairHostFixture(t)
	u.setup.PRRepairCustody = newPRRepairCatalog(u.setup, true)
	c := prRepairCustodian{layout: u.l, setup: u.setup, runID: source.Identity.RunID}
	entered, finish := make(chan struct{}), make(chan struct{})
	effect := make(chan error, 1)
	go func() {
		effect <- c.scope(t.Context(), *input.Target, func(context.Context) error { close(entered); <-finish; return nil })
	}()
	<-entered
	manager, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	next := &schedulerDefinitions{Set: &instance.ConfigSet{Gaggles: []apiv1.Gaggle{source.RetainedGaggle}, Workflows: []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Readiness: apiv1.ReadinessConditions{ClaimVisibility: "shared"}}}}}, Worktrees: manager, WorktreesByGaggle: map[string]*worktree.Manager{"next": manager}}
	reload := &configReloader{setup: u.setup}
	publishing, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- reload.publishInteractiveDefinitions(next.Set, func() error {
			return reload.publishPRRepairDefinitions(next, func() error { close(publishing); return nil })
		})
	}()
	select {
	case <-publishing:
		t.Fatal("publication crossed active repair")
	case <-time.After(30 * time.Millisecond):
	}
	if source.Lease.Context().Err() != nil {
		t.Fatal("unchanged gaggle was revoked")
	}
	// These fields are intentionally mutable during reload. The effect's catalog
	// owns copied topology and manager membership, so these writes cannot race it.
	u.setup.Definitions = next.Set
	u.setup.Worktrees = manager
	u.setup.WorktreesByGaggle = map[string]*worktree.Manager{"replacement": manager}
	close(finish)
	if err := <-effect; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := u.setup.PRRepairCustody.use(t.Context(), func(snapshot prRepairSnapshot) error {
		if snapshot.enabled || snapshot.managers[0] != manager {
			t.Fatal("mixed catalog", snapshot)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Mutating input membership after publication must not mutate its snapshot.
	delete(next.WorktreesByGaggle, "next")
	if err := u.setup.PRRepairCustody.use(t.Context(), func(snapshot prRepairSnapshot) error {
		if len(snapshot.managers) != 3 {
			t.Fatal(snapshot)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPRRepairChangedPolicyJoinsBeforeCatalogPublication(t *testing.T) {
	u, source, input := repairHostFixture(t)
	u.setup.PRRepairCustody = newPRRepairCatalog(u.setup, true)
	c := prRepairCustodian{layout: u.l, setup: u.setup, runID: source.Identity.RunID}
	entered, joined := make(chan struct{}), make(chan struct{})
	effect := make(chan error, 1)
	go func() {
		effect <- c.scope(t.Context(), *input.Target, func(context.Context) error {
			close(entered)
			<-source.Lease.Context().Done()
			close(joined)
			return nil
		})
		source.Lease.Close()
	}()
	<-entered
	changed := *source.RetainedGaggle.DeepCopy()
	changed.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"session.message"}
	next := &schedulerDefinitions{Set: &instance.ConfigSet{Gaggles: []apiv1.Gaggle{changed}}, WorktreesByGaggle: u.setup.WorktreesByGaggle}
	reload := &configReloader{setup: u.setup}
	if err := reload.publishInteractiveDefinitions(next.Set, func() error {
		return reload.publishPRRepairDefinitions(next, func() error {
			select {
			case <-joined:
			default:
				t.Fatal("publication occurred before cancelled writer joined")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-effect; err != nil {
		t.Fatal(err)
	}
}

func TestPRRepairCatalogFailedPublicationAndBoundedAdmission(t *testing.T) {
	u, _, _ := repairHostFixture(t)
	catalog := newPRRepairCatalog(u.setup, true)
	u.setup.PRRepairCustody = catalog
	reload := &configReloader{setup: u.setup}
	failed := errors.New("scheduler refused")
	if err := reload.publishPRRepairDefinitions(&schedulerDefinitions{}, func() error { return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if err := catalog.use(t.Context(), func(snapshot prRepairSnapshot) error {
		if !snapshot.enabled {
			t.Fatal("failed publication changed catalog")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	catalog.mu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Millisecond)
	defer cancel()
	err := catalog.use(ctx, func(prRepairSnapshot) error { t.Fatal("reader crossed publication"); return nil })
	catalog.mu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestPRRepairCustodyDoesNotReadMutableSetupDuringReload(t *testing.T) {
	u, source, input := repairHostFixture(t)
	u.setup.PRRepairCustody = newPRRepairCatalog(u.setup, true)
	original := u.setup.Definitions
	manager := u.setup.WorktreesByGaggle["gaggle"]
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for ctx.Err() == nil {
			u.setup.Definitions = nil
			u.setup.Worktrees = nil
			u.setup.WorktreesByGaggle = nil
			u.setup.Definitions = original
			u.setup.Worktrees = manager
			u.setup.WorktreesByGaggle = map[string]*worktree.Manager{"gaggle": manager}
			runtime.Gosched()
		}
	}()
	defer func() { cancel(); <-stopped }()
	c := prRepairCustodian{layout: u.l, setup: u.setup, runID: source.Identity.RunID}
	for range 10 {
		if err := c.scope(t.Context(), *input.Target, func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
}
