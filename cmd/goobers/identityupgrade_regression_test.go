package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

func writeIdentityUpgradeFixture(t *testing.T, root, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func migrateIdentityUpgradeFixture(t *testing.T, root, canonical, legacy string) {
	t.Helper()
	writeIdentityUpgradeFixture(t, root, instance.RootIdentityFileName, canonical+"\n")
	writeIdentityUpgradeFixture(t, root, "instance-id", legacy+"\n")
	if _, err := instance.NewLayout(root).EnsureIdentity(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, err := instance.ResolveRootIdentity(root, legacy); err != nil || got != canonical {
		t.Fatalf("alias setup: %q %v", got, err)
	}
}

func TestMigratedPendingChildRemainsAdmissible(t *testing.T) {
	f := newPinnedChildFixture(t)
	f.load(t)
	legacy := f.parent.InstanceID
	canonical := strings.Repeat("c", 32)
	migrateIdentityUpgradeFixture(t, f.layout.Root, canonical, legacy)
	if _, err := loadPinnedChildStage(t.Context(), f.layout, f.cfg, f.applied, f.parent, "plan"); err != nil {
		t.Fatalf("migration stranded previously admitted parent: %v", err)
	}
}

type identityUpgradeEpochReader struct {
	id string
}

func (r identityUpgradeEpochReader) OpenClaimEpochs(context.Context, providers.RepositoryRef, string) ([]providers.ClaimEpoch, error) {
	return []providers.ClaimEpoch{{Trusted: true, RunID: "old-owned-run", InstanceID: r.id}}, nil
}

func TestMigratedClaimRetainsOwnership(t *testing.T) {
	root := t.TempDir()
	canonical, legacy := strings.Repeat("a", 32), strings.Repeat("b", 32)
	migrateIdentityUpgradeFixture(t, root, canonical, legacy)
	session := backlogClaimSession{env: backlogQueryEnv{root: root, layout: instance.NewLayout(root)}}
	t.Setenv(executor.InstanceIDEnvVar, legacy)
	if _, owned, err := session.ownTrustedClaimEpoch(t.Context(), identityUpgradeEpochReader{legacy}, "1", "old-owned-run"); err != nil || !owned {
		t.Fatalf("control: owned=%v err=%v", owned, err)
	}
	t.Setenv(executor.InstanceIDEnvVar, canonical)
	if _, owned, err := session.ownTrustedClaimEpoch(t.Context(), identityUpgradeEpochReader{legacy}, "1", "old-owned-run"); err != nil || !owned {
		t.Fatalf("migration lost proven local alias claim: owned=%v err=%v", owned, err)
	}
}

func TestTelemetryLookupByEitherMigratedIdentity(t *testing.T) {
	root := t.TempDir()
	layout := instance.NewLayout(root)
	canonical, legacy := strings.Repeat("a", 32), strings.Repeat("b", 32)
	migrateIdentityUpgradeFixture(t, root, canonical, legacy)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := os.MkdirAll(filepath.Dir(layout.TelemetryDB()), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := rollup.Open(layout.TelemetryDB())
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{legacy, canonical} {
		runID, pullID := []string{"before-upgrade", "after-upgrade"}[i], []string{"1", "2"}[i]
		run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{
			RunID: runID, Workflow: "landing", Gaggle: "web", InstanceID: id,
		}, nil, journal.WithClock(func() time.Time { return at }))
		if err != nil {
			t.Fatal(err)
		}
		confirmation := &providers.MergeConfirmation{
			RepositoryAPIURL: "https://forge.example/repos/acme/app", PullID: pullID, MergeSHA: "commit",
		}
		if err := run.Append(journal.Event{
			Type:        journal.EventRefTouched,
			ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: pullID},
			Runner:      providers.MutationRunnerFields("merge", confirmation, nil, nil),
		}); err != nil {
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
		if err := db.IngestRun(t.Context(), run.Dir()); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", legacy, canonical} {
		args := []string{
			"telemetry", "merges", "--json",
			"--since=2026-09-01T00:00:00Z", "--until=2026-09-02T00:00:00Z",
		}
		if id != "" {
			args = append(args, "--instance-id="+id)
		}
		code, out, stderr := runArgs(t, append(args, root)...)
		if code != 0 {
			t.Fatalf("query: %d %s", code, stderr)
		}
		var report rollup.MergeReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Merges) != 2 {
			t.Errorf("lookup %q splits one migrated root's history: got %d merges, want 2", id, len(report.Merges))
		}
	}
}

func TestConcurrentLegacyIdentityAdoption(t *testing.T) {
	for _, dual := range []bool{false, true} {
		root := t.TempDir()
		legacy, canonical := strings.Repeat("b", 32), strings.Repeat("b", 32)
		writeIdentityUpgradeFixture(t, root, "instance-id", legacy+"\n")
		writeIdentityUpgradeFixture(t, root, "instance.yaml", "config\n")
		if dual {
			canonical = strings.Repeat("a", 32)
			writeIdentityUpgradeFixture(t, root, instance.RootIdentityFileName, canonical+"\n")
		}
		var wg sync.WaitGroup
		for i := range 32 {
			wg.Go(func() {
				var (
					got string
					err error
				)
				if i%2 == 0 {
					got, err = instance.EnsureRootIdentity(t.Context(), root)
				} else {
					got, err = instance.NewLayout(root).EnsureIdentity(t.Context())
				}
				if err != nil || got != canonical {
					t.Errorf("adoption: %q %v", got, err)
				}
			})
		}
		wg.Wait()
		if dual {
			if got, err := instance.ResolveRootIdentity(root, legacy); err != nil || got != canonical {
				t.Fatalf("alias: %q %v", got, err)
			}
		}
	}
}

func TestCorruptIdentityAliasFailsClosed(t *testing.T) {
	for _, legacy := range []string{"", strings.Repeat("a", 32)} {
		root := t.TempDir()
		writeIdentityUpgradeFixture(t, root, instance.RootIdentityFileName, strings.Repeat("a", 32)+"\n")
		if legacy != "" {
			writeIdentityUpgradeFixture(t, root, "instance-id", legacy+"\n")
		}
		writeIdentityUpgradeFixture(t, root, instance.RootIdentityAliasesFileName, "corrupt\n")
		if _, err := instance.NewLayout(root).EnsureIdentity(t.Context()); err == nil {
			t.Errorf("corrupt alias accepted with legacy=%q", legacy)
		}
	}
}

func TestZeroRootIdentityFailsClosed(t *testing.T) {
	root := t.TempDir()
	writeIdentityUpgradeFixture(t, root, instance.RootIdentityFileName, strings.Repeat("0", 32)+"\n")
	if id, err := instance.NewLayout(root).EnsureIdentity(t.Context()); err == nil {
		t.Fatalf("invalid canonical attribution ID accepted: %q", id)
	}
}
