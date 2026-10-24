//go:build cloudintegration

package triggers

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/pgtest"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/startintent"
)

const instance = "triggers-instance"

func rolePool(t *testing.T, owner *pgxpool.Pool, role string) *pgxpool.Pool {
	t.Helper()
	config := owner.Config()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var current string
	if err := pool.QueryRow(t.Context(), "SELECT current_user").Scan(&current); err != nil || current != role {
		t.Fatalf("current_user = %q, %v; want %s", current, err, role)
	}
	return pool
}

// replicaService is one API replica with a REAL clock: every attempt carries a
// different deadline, which is what production retries look like.
func replicaService(t *testing.T, owner *pgxpool.Pool, admission *fakeAdmission) *Service {
	t.Helper()
	store, err := acceptance.New(rolePool(t, owner, "goobers_api"), instance)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := newService(store, admission)
	service.Now = nil
	return service
}

func TestTriggersUnderAPIRole(t *testing.T) {
	owner := pgtest.Pool(t, "triggers")
	admission := &fakeAdmission{generation: "gen-1"}
	first, second := replicaService(t, owner, admission), replicaService(t, owner, admission)

	// Concurrent same-key requests across two replicas: one acceptance.
	const attempts = 16
	ids := make([]string, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			service := first
			if i%2 == 1 {
				service = second
			}
			response, err := service.Trigger(t.Context(), request())
			ids[i], errs[i] = response.AcceptanceID, err
		}()
	}
	wg.Wait()
	for i := range attempts {
		if errs[i] != nil || ids[i] != ids[0] || ids[0] == "" {
			t.Fatalf("attempt %d: id %q err %v, want the single acceptance %q", i, ids[i], errs[i], ids[0])
		}
	}
	var rows int
	if err := rolePool(t, owner, "goobers_api").QueryRow(t.Context(),
		"SELECT count(*) FROM control.acceptance WHERE instance_id=$1", instance).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("acceptance rows = %d, %v; want 1", rows, err)
	}

	// A retry after a newer generation is applied, and after the workflow stops
	// being eligible, still returns the original acceptance (real clock, PG
	// payload comparison).
	admission.setGeneration("gen-2")
	admission.refuse = map[string]bool{"review": true}
	if again, err := second.Trigger(t.Context(), request()); err != nil || !again.Duplicate || again.AcceptanceID != ids[0] {
		t.Fatalf("retry after reconfiguration = %+v, %v; want the original acceptance", again, err)
	}
	admission.refuse = nil

	// Key reuse for a different request is a conflict, never a second row.
	changed := request()
	changed.Workflow = "triage"
	if _, err := second.Trigger(t.Context(), changed); status(err) != 409 {
		t.Fatalf("conflicting reuse = %v, want 409", err)
	}

	// Status is actor scoped through the API grants.
	got, err := first.TriggerStatus(t.Context(), httpapi.TriggerStatusRequest{AcceptanceID: ids[0], Actor: "operator"})
	if err != nil || got.State != "accepted" {
		t.Fatalf("status = %+v, %v", got, err)
	}
	if _, err := first.TriggerStatus(t.Context(), httpapi.TriggerStatusRequest{AcceptanceID: ids[0], Actor: "intruder"}); status(err) != 404 {
		t.Fatalf("other actor = %v, want 404", err)
	}

	// The controller (its own role, no API credentials) sees a start envelope it
	// can resolve: the pinned generation and digests round-trip.
	controller, err := acceptance.New(rolePool(t, owner, "goobers_controller"), instance)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := controller.Pending(t.Context(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("controller pending = %v, %v", pending, err)
	}
	envelope, err := startintent.Parse(pending[0].Payload)
	if err != nil || envelope.Target.ConfigGeneration != "gen-1" || envelope.Target.WorkflowDigest != "wf-gen-1" {
		t.Fatalf("controller envelope = %+v, %v", envelope, err)
	}
}
