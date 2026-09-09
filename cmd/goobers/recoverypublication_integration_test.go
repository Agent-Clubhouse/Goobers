//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRecoveryPublicationTakesVerifiedHostCustody(t *testing.T) {
	testdep.Require(t, "git")
	for _, mode := range []string{"live", "released-during-transfer", "finished-during-transfer"} {
		t.Run(mode, func(t *testing.T) {
			testRecoveryPublicationCustody(t, mode)
		})
	}
}

func testRecoveryPublicationCustody(t *testing.T, mode string) {
	t.Helper()
	release := mode == "released-during-transfer"
	ctx := context.Background()
	layout := instance.NewLayout(initDemo(t))
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderKind(cfg.Repos[0].Provider), Owner: cfg.Repos[0].Owner, Name: cfg.Repos[0].Name}
	const runID = "publication-worker"
	started := time.Now().UTC().Add(-60 * 24 * time.Hour)
	finished := time.Now().UTC().Add(-time.Hour)
	run, err := journal.Create(layout.RunsDir(), journal.RunIdentity{Schema: journal.RunSchema, RunID: runID, Workflow: "implementation", WorkflowVersion: 1, StartedAt: started}, nil, journal.WithClock(func() time.Time { return finished }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("7", runID, "implementation", time.Hour); err != nil || !ok {
		t.Fatalf("claim: %t %v", ok, err)
	}
	seedItemRepositoryForTest(t, layout, runID, "7", repo)
	source := t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	recoveryCLIGit(t, source, "commit", "--allow-empty", "-m", "base")
	manager, err := worktree.NewManager(t.TempDir(), worktree.WithRemoteGitGate(func(context.Context, string) error {
		t.Fatal("archive publication attempted a forge operation")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	previous := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) { return source, nil }
	t.Cleanup(func() { repoCloneURL = previous })
	if err := os.WriteFile(filepath.Join(source, "implementation.txt"), []byte("remote worker changes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Deliberately disagree with the host clock and retention policy.
	prepared, err := recovery.PrepareRecord(ctx, source, repo.CanonicalKey(), runID, "main", started.Add(time.Minute), started.Add(365*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	archive := t.TempDir()
	record, err := recovery.PublishRetainedState(ctx, source, archive, []string{source}, prepared, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if err := recovery.WriteArchiveEnvelope(ctx, filepath.Join(archive, recovery.BundleFileName), record, 1<<20, &wire); err != nil {
		t.Fatal(err)
	}
	service := recoveryDeliveryService{layout: layout, setup: &schedulerSetup{LegacyWorktrees: manager}}
	var body io.Reader = bytes.NewReader(wire.Bytes())
	if release {
		body = &recoveryPublicationHookReader{Reader: body, beforeRead: func() error { return ledger.Release("7", runID) }}
	}
	if mode == "finished-during-transfer" {
		body = &recoveryPublicationHookReader{Reader: body, beforeRead: func() error {
			if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)}); err != nil {
				return err
			}
			// Terminal renewal scans an empty inventory while the archive is
			// still on the wire. Intake must close this ordering gap itself.
			return renewTerminalRecovery(layout, runID)
		}}
	}
	err = service.PublishRecovery(ctx, runID, repo.CanonicalKey(), "7", body)
	if (err != nil) != release {
		t.Fatalf("publication release=%t: %v", release, err)
	}
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	captures, err := recovery.RecordsFromEvents(events, runID)
	if err != nil {
		t.Fatal(err)
	}
	if release {
		if len(captures) != 0 {
			t.Fatalf("released claim acknowledged: %+v", captures)
		}
		return
	}
	deadline := started.Add(30 * 24 * time.Hour)
	if mode == "finished-during-transfer" {
		deadline = finished.Add(30 * 24 * time.Hour)
	}
	if len(captures) != 1 || !captures[0].CreatedAt.Equal(started) || !captures[0].RetainUntil.Equal(deadline) {
		t.Fatalf("host custody policy: %+v", captures)
	}
	found, err := manager.WithExistingMirror(ctx, source, func(mirror string) error {
		if got := recoveryCLIGit(t, mirror, "show", captures[0].Ref+":implementation.txt"); got != "remote worker changes" {
			t.Fatalf("host ref content: %q", got)
		}
		return nil
	})
	if err != nil || !found {
		t.Fatalf("missing managed custody repository: found=%t err=%v", found, err)
	}
	verifyRecoveryPublicationArchive(t, service, wire.Bytes(), runID, repo.CanonicalKey(), deadline)
}

func verifyRecoveryPublicationArchive(t *testing.T, service recoveryDeliveryService, wire []byte, runID, key string, deadline time.Time) {
	t.Helper()
	ctx := context.Background()
	publishRecoveryOverAuthenticatedHTTP(t, service, wire, runID)
	entries, err := recovery.ReadInventory(ctx, filepath.Join(service.layout.Root, "recovery"), 128)
	if err != nil || len(entries) != 1 {
		t.Fatalf("retry inventory: %+v %v", entries, err)
	}
	entry := entries[0]
	entry.Record, err = recovery.ReadRetainedRecord(entry.RecordPath)
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Record.RetainUntil.Equal(deadline) {
		t.Fatalf("retry changed durable retention deadline: got=%s want=%s", entry.Record.RetainUntil, deadline)
	}
	destination := t.TempDir()
	recoveryCLIGit(t, destination, "init", "--bare")
	if err := recovery.ImportSnapshotBundle(ctx, destination, filepath.Join(filepath.Dir(entry.RecordPath), recovery.BundleFileName), entry.Record, 1<<20); err != nil {
		t.Fatal(err)
	}
	if got := recoveryCLIGit(t, destination, "show", entry.Record.Ref+":implementation.txt"); got != "remote worker changes" {
		t.Fatalf("independent host archive content: %q", got)
	}
}

func publishRecoveryOverAuthenticatedHTTP(t *testing.T, service recoveryDeliveryService, wire []byte, runID string) {
	t.Helper()
	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator), httpapi.WithRecoveryService(service))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	archive := t.TempDir()
	record, err := recovery.ReceiveArchiveEnvelope(context.Background(), bytes.NewReader(wire), archive, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{podauth.ScopeJournal, podauth.ScopeClaims, "runtime"} {
		var token string
		if scope == "runtime" {
			// The dispatch parent already has this run-scoped runtime token;
			// it is never passed through to the stage subprocess.
			token, err = registry.Mint(runID, time.Hour)
		} else {
			token, err = registry.MintScoped(runID, time.Hour, scope)
		}
		if err != nil {
			t.Fatal(err)
		}
		publisher := recovery.HTTPArchivePublisher{BaseURL: server.URL, Token: token, RunID: runID}
		err = publisher.PublishArchive(context.Background(), "7", record, filepath.Join(archive, recovery.BundleFileName))
		if (err != nil) != (scope == podauth.ScopeJournal) {
			t.Fatalf("authenticated %s upload: %v", scope, err)
		}
	}
}

type recoveryPublicationHookReader struct {
	io.Reader
	beforeRead func() error
}

func (r *recoveryPublicationHookReader) Read(p []byte) (int, error) {
	if r.beforeRead != nil {
		beforeRead := r.beforeRead
		r.beforeRead = nil
		if err := beforeRead(); err != nil {
			return 0, err
		}
	}
	return r.Reader.Read(p)
}
