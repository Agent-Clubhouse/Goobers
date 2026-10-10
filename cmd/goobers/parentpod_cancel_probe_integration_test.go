//go:build integration

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

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

// Preserve the host-owned cause in failed qualification output before TempDir
// cleanup removes the disposable journal. Never infer cancellation from a flag.
func logQualificationChildOutcome(t *testing.T, layout instance.Layout, runID string) {
	t.Helper()
	dir, err := layout.FindRunDir(runID)
	if err != nil {
		t.Log("child diagnostic journal", err)
		return
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Log("child diagnostic reader", err)
		return
	}
	events, err := reader.Events()
	if err != nil {
		t.Log("child diagnostic events", err)
		return
	}
	for _, event := range events {
		if event.Error != nil || event.TerminalCause != nil || event.Type == journal.EventStageFinished || event.Type == journal.EventRunFinished {
			t.Logf("child %s stage=%s attempt=%d status=%s reason=%s error=%+v cause=%+v outputs=%+v", event.Type, event.Stage, event.Attempt, event.Status, event.Reason, event.Error, event.TerminalCause, event.Outputs)
		}
	}
}

func TestIntegrationContainedParentCancellationFencePrecedesQueueDelivery(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	qualifyContainedParentJourney(t, "cancel-fence-first")
}

// Hold the next queue sweep until the worker has observed the durable fence.
// This is a real delivery ordering, not an injected terminal result.
func awaitQualificationFencedChild(t *testing.T, ctx context.Context, registry *daemonRunnerRegistry, queue *triggerqueue.Store, parentRunID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		children, err := queue.Children(ctx, triggerqueue.ChildParent{Gaggle: "example", ParentRunID: parentRunID}, "", 10)
		if err != nil || len(children) != 1 {
			t.Fatal("find fenced child", children, err)
		}
		if _, live := registry.Resolve(children[0].RunID, "example", nil); !live {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("worker owner did not release after family fence", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
