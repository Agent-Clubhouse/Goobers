//go:build cloudintegration

package schema_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/cancelpg"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/recoverypg"
	"github.com/goobers/goobers/internal/cloud/terminalreportpg"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

func smokePool(t *testing.T, databaseURL, role string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.MaxConns = lease.MaxFencedConnections
	config.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+role)
		return err
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRuntimeRolePathsUnderInventory(t *testing.T) {
	_, databaseURL := migratedPool(t)
	api, controller := smokePool(t, databaseURL, "goobers_api"), smokePool(t, databaseURL, "goobers_controller")
	manager, err := lease.New(lease.Config{Pool: controller, InstanceID: "instance", Name: "control", Holder: "control"})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := manager.Acquire(t.Context())
	if err != nil {
		t.Fatalf("controller lease acquire: %v", err)
	}
	now := time.Now()

	apiAcceptance, err := acceptance.New(api, "instance")
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := apiAcceptance.Accept(t.Context(), "k1", "alice", []byte("payload"), now)
	if err != nil {
		t.Fatalf("api Accept: %v", err)
	}
	if _, replay, err := apiAcceptance.Accept(t.Context(), "k1", "alice", []byte("payload"), now); err != nil || !replay {
		t.Fatalf("api Accept replay = %v, %v", replay, err)
	}
	if _, err := apiAcceptance.Get(t.Context(), record.ID, "alice"); err != nil {
		t.Fatalf("api Get: %v", err)
	}
	ctlAcceptance, err := acceptance.New(controller, "instance")
	if err != nil {
		t.Fatal(err)
	}
	ctlAcceptance.BindFence(manager, handle)
	if _, err := ctlAcceptance.Pending(t.Context(), 10); err != nil {
		t.Fatalf("controller Pending: %v", err)
	}
	if err := ctlAcceptance.BeginDispatch(t.Context(), record.ID); err != nil {
		t.Fatalf("controller BeginDispatch: %v", err)
	}
	runID := strings.TrimPrefix(record.ID, "trigger-")
	if err := ctlAcceptance.RecordDispatch(t.Context(), record.ID, runID); err != nil {
		t.Fatalf("controller RecordDispatch: %v", err)
	}
	if err := ctlAcceptance.Finish(t.Context(), record.ID, triggerqueue.Dispatched, runID, "", now); err != nil {
		t.Fatalf("controller Finish: %v", err)
	}

	outbox := terminalreportpg.Store{Fence: manager}
	effect := terminalreport.Effect{Kind: terminalreport.WorkFailure, Repository: providers.RepositoryRef{Provider: providers.ProviderADO, Name: "app"}, ItemID: "1", RunID: "run-1", Count: 1}
	if _, err := outbox.Publish(t.Context(), handle, effect); err != nil {
		t.Fatalf("controller outbox Publish (sequence): %v", err)
	}
	deliveries, err := outbox.Claim(t.Context(), handle, 1)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("controller outbox Claim = %d, %v", len(deliveries), err)
	}
	if err := outbox.Delivered(t.Context(), handle, deliveries[0]); err != nil {
		t.Fatalf("controller outbox Delivered: %v", err)
	}

	receipts := cancelpg.Store{Fence: manager}.Bind(handle)
	if _, _, err := receipts.Begin(t.Context(), "c1", "alice", []byte("cancel")); err != nil {
		t.Fatalf("controller cancel Begin: %v", err)
	}
	if err := receipts.Finish(t.Context(), "c1", []byte("ok")); err != nil {
		t.Fatalf("controller cancel Finish: %v", err)
	}

	ref, _ := recovery.RefForRun("run-1")
	sum := sha256.Sum256([]byte("bundle"))
	r := recovery.Record{Version: 1, RunID: "run-1", Ref: ref,
		RepositoryKey: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"}.CanonicalKey(),
		BaseSHA:       strings.Repeat("a", 40), SnapshotSHA: strings.Repeat("b", 40), PatchDigest: "sha256:" + strings.Repeat("c", 64),
		ArchiveDigest: fmt.Sprintf("sha256:%x", sum), ArchiveBytes: 6, ArchiveFormat: "full", CreatedAt: now.Add(-time.Minute)}
	index := recoverypg.Store{Pool: controller, Fence: manager}.Bind(handle)
	if _, err := index.Publish(t.Context(), r, "bundles/run-1", time.Millisecond); err != nil {
		t.Fatalf("controller recovery Publish: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := index.Reap(t.Context(), 10); err != nil {
		t.Fatalf("controller recovery Reap: %v", err)
	}

	var active int
	if err := api.QueryRow(t.Context(), "SELECT active_version FROM control.readmodel_version LIMIT 1").Scan(&active); err != nil {
		t.Fatalf("api read-model version read: %v", err)
	}
}
