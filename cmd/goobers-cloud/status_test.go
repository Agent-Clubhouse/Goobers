package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	cloudmigrate "github.com/goobers/goobers/internal/cloud/migrate"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
)

func TestDatabaseStatusCommandsUseReadOnlyBackends(t *testing.T) {
	originalPools, originalPing, originalCheck := newCloudPools, pingCloudPools, checkCloudSchema
	originalMigration, originalLease, originalSignal := loadMigrationStatus, loadLeaseStatus, signalContext
	t.Cleanup(func() {
		newCloudPools, pingCloudPools, checkCloudSchema = originalPools, originalPing, originalCheck
		loadMigrationStatus, loadLeaseStatus, signalContext = originalMigration, originalLease, originalSignal
	})
	t.Setenv("GOOBERS_DATABASE_URL", "postgres://localhost/goobers")
	t.Setenv("GOOBERS_DATABASE_DIRECT_URL", "postgres://localhost/goobers")
	signalContext = func() (context.Context, func()) { return context.Background(), func() {} }
	newCloudPools = func(context.Context, cloudpg.Config) (*cloudpg.Pools, error) { return &cloudpg.Pools{}, nil }
	pingCloudPools = func(context.Context, *cloudpg.Pools) (string, error) { return "", nil }
	checkCloudSchema = func(context.Context, *pgxpool.Pool, string) error { return nil }
	loadMigrationStatus = func(context.Context, *pgxpool.Pool, string) (cloudmigrate.StatusReport, error) {
		return cloudmigrate.StatusReport{Binary: "0.7.0", Compatible: true, MinimumCompatibleBinary: "0.6.0", Applied: []cloudmigrate.Applied{}}, nil
	}
	loadLeaseStatus = func(context.Context, *pgxpool.Pool) (cloudlease.StatusReport, error) {
		return cloudlease.StatusReport{ObservedAt: time.Unix(1, 0).UTC(), Leases: []cloudlease.Current{}, History: []cloudlease.HistoryEntry{}}, nil
	}

	code, stdout, stderr := runForTest("migrate", "status")
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"compatible":true`) || !strings.Contains(stdout, `"minimumCompatibleBinary":"0.6.0"`) {
		t.Fatalf("migrate status: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, stderr = runForTest("lease", "status")
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"leases":[]`) || !strings.Contains(stdout, `"history":[]`) {
		t.Fatalf("lease status: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
