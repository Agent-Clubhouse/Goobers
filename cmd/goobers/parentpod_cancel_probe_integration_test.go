//go:build integration

package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// This test-only endpoint proves that the generated shell actually began. It
// supplies no runtime authority and is never used as stopped-writer evidence.
func parentQualificationCancellationProbe(t *testing.T, repository string) <-chan struct{} {
	t.Helper()
	started := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		once.Do(func() { close(started) })
	}))
	t.Cleanup(server.Close)
	endpoint := "http://host.docker.internal:" + strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port) + "/started"
	if err := os.WriteFile(filepath.Join(repository, "qualification-notify"), []byte(endpoint), 0600); err != nil {
		t.Fatal(err)
	}
	return started
}

// Cancellation stops execution, but does not silently acknowledge or discard an
// unresolved child result. Prove that the exact parent checkout stays protected.
func assertCancelledParentRetained(t *testing.T, f pinnedChildFixture, runID string, child triggerqueue.ChildRecord) {
	t.Helper()
	if !child.AcknowledgedAt.IsZero() {
		t.Fatal("cancellation acknowledged the child result without a disposition")
	}
	dir, err := f.layout.FindRunDir(runID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyParentArchiveChildren(t.Context(), f.layout, id); !errors.Is(err, invoke.ErrChildCustodyPending) {
		t.Fatal("unacknowledged child no longer protects parent cleanup", err)
	}
	pending, err := runner.ParentRetirementCandidates(reader)
	if err != nil || len(pending) != 1 || pending[0].RetirementSeq != 0 {
		t.Fatal("parent custody was retired", pending, err)
	}
	layout, err := instance.EffectiveWorkcopiesLayout(f.layout.ForGaggle(id.Gaggle), f.cfg, &f.applied.Gaggles[0])
	if err != nil {
		t.Fatal(err)
	}
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		t.Fatal(err)
	}
	url, err := childRepoCloneURL(f.applied.Gaggles[0].Spec.Project)
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := manager.AdoptHeldStage(t.Context(), url, pending[0].Workspace.Custody.Workspace)
	if err != nil {
		t.Fatal("parent workspace lost its exact hold", err)
	}
	data, err := os.ReadFile(filepath.Join(checkout.Path, "parent-before-child.txt"))
	if err != nil || string(data) != "parent before child\n" {
		t.Fatal("cancellation lost parent work", string(data), err)
	}
}

func TestIntegrationContainedParentCancellationStopsAuthoredChild(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	qualifyContainedParentJourney(t, "cancel")
}
