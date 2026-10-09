//go:build integration

package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace"
	"github.com/goobers/goobers/internal/parallelworkspace/spec"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

type forkCaptureFault struct {
	*journal.Run
	checkpoint string
}

func (f forkCaptureFault) Append(event journal.Event) error {
	if f.checkpoint == "no-preparation" {
		return errors.New("injected preparation append failure")
	}
	if err := f.Run.Append(event); err != nil {
		return err
	}
	if f.checkpoint == "before-pin" {
		return errors.New("injected stop after durable preparation")
	}
	return nil
}

func (f forkCaptureFault) RecordArtifactBoundedWithIntegrity(name string, data []byte, integrity apiv1.Integrity, limit int) (journal.Ref, error) {
	if f.checkpoint == "after-pin" && name == "parallel-source.bundle" {
		return journal.Ref{}, errors.New("injected stop after Git pin before carrier publication")
	}
	return f.Run.RecordArtifactBoundedWithIntegrity(name, data, integrity, limit)
}

func verifyInterruptedForkCapture(t *testing.T, f pinnedChildFixture, run *journal.Run, reader *journal.Reader, layout instance.Layout, manager *worktree.Manager, backend parallelworkspace.Service, request spec.Request, checkpoint string) {
	t.Helper()
	writeFileContent(t, filepath.Join(request.Workspace, "orphan.txt"), "unpublished source work\n")
	head := recoveryCLIGit(t, request.Workspace, "rev-parse", "HEAD")
	index := recoveryCLIGit(t, request.Workspace, "write-tree")
	policy, err := backend.Policy(request.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	project := request.Repository
	projectKey := (providers.RepositoryRef{Provider: providers.ProviderKind(project.Provider), URL: project.BaseURL, Owner: project.Owner, Project: project.Project, Name: project.Name}).CanonicalKey()
	expected, err := recovery.CaptureChildSnapshot(t.Context(), request.Workspace, projectKey, request.RunID, request.At, request.At.Add(30*24*time.Hour), policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Prepare(t.Context(), forkCaptureFault{Run: run, checkpoint: checkpoint}, request, nil); err == nil {
		t.Fatal("capture failure injection did not stop publication")
	}
	url, err := backend.CloneURL(request.Repository)
	if err != nil {
		t.Fatal(err)
	}
	verifyForkSourcePin(t, manager, url, expected.Record, checkpoint == "after-pin")
	pending, err := parallelworkspace.PendingPreparations(reader)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint == "no-preparation" {
		if len(pending) != 0 {
			t.Fatal("failed append invented capture ownership")
		}
		return
	}
	if len(pending) != 1 || pending[0].Value.Snapshot.Record != expected.Record {
		t.Fatal("interrupted capture lost exact ownership", pending)
	}
	if err := preserveParentWorkspaceJournal(retention.Result{RunDir: run.Dir(), RunID: request.RunID}); !errors.Is(err, retention.ErrCustodyHeld) {
		t.Fatal("preparation journal could be pruned", err)
	}
	queue, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	service := newDaemonCredentialService(f.layout, f.cfg, nil, journal.NewRegistryScrubber(), nil).withStageGrants(f.layout.Root, "127.0.0.1:8080", false)
	t.Cleanup(func() { unregisterDaemonStageGrants(f.layout.Root, service) })
	if err := service.enableChildWorkflows(queue, f.applied); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseAborted)}); err != nil {
		t.Fatal(err)
	}
	if err := releaseTerminalParentArchives(layout, manager, request.RunID); !errors.Is(err, journal.ErrRecoveryBusy) {
		t.Fatal("orphan preparation borrowed live writer", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := releaseTerminalParentArchives(layout, manager, request.RunID); err != nil {
		t.Fatal("startup did not retain orphan preparation", err)
	}
	pending, err = parallelworkspace.PendingPreparations(reader)
	if err != nil || len(pending) != 0 {
		t.Fatal("capture ownership not settled", pending, err)
	}
	entries, err := recovery.ReadInventory(t.Context(), filepath.Join(layout.Root, "recovery"), 100)
	if err != nil || len(entries) != 1 || entries[0].Record.Ref != expected.Record.Ref || entries[0].Record.ArchiveDigest == "" {
		t.Fatal("orphan source not transferred into durable inventory", entries, err)
	}
	verifyForkSourcePin(t, manager, url, expected.Record, true)
	if got := recoveryCLIGit(t, request.Workspace, "show", expected.Record.SnapshotSHA+":orphan.txt"); got != "unpublished source work" {
		t.Fatal("unpublished work lost", got)
	}
	if recoveryCLIGit(t, request.Workspace, "rev-parse", "HEAD") != head || recoveryCLIGit(t, request.Workspace, "write-tree") != index {
		t.Fatal("capture cleanup changed source Git state")
	}
	if err := releaseTerminalParentArchives(layout, manager, request.RunID); err != nil {
		t.Fatal("preparation cleanup retry", err)
	}
}
