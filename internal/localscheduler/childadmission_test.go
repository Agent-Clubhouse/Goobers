package localscheduler

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func childAdmissionFixture() (WorkflowEntry, ChildAdmissionRequest) {
	parent := WorkflowEntry{Workflow: "parent", Gaggle: "own", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub}, Readiness: apiv1.ReadinessConditions{MaxConcurrentRuns: 1, MaxRunsPerHour: 2}}
	request := ChildAdmissionRequest{RunID: strings.Repeat("b", 32), ParentRunID: strings.Repeat("a", 32), Parent: entryIdentity(parent), Child: WorkflowEntry{Workflow: "generated", Gaggle: "own", RepoRef: parent.RepoRef}}
	return parent, request
}
func requireChildRefusal(t *testing.T, err error, reason string) {
	t.Helper()
	var refused *TriggerRejectedError
	if !errors.As(err, &refused) || refused.Reason != reason {
		t.Fatalf("refusal=%v want %s", err, reason)
	}
}

func TestChildAdmissionSharesParentCapacityWithoutImplicitTransfer(t *testing.T) {
	parent, request := childAdmissionFixture()
	s, _ := newTestScheduler(t, []WorkflowEntry{parent})
	parentRelease, ok, reason := s.ReserveContinuation(request.ParentRunID, parent.Gaggle, parent.Workflow)
	if !ok {
		t.Fatal(reason)
	}
	_, err := s.ReserveChild(t.Context(), request, time.Now())
	requireChildRefusal(t, err, ReasonMaxParallel)
	if s.conditions.ActiveWorkflow(request.Parent) != 1 {
		t.Fatal("child stole parent's permit")
	}
	// Only the caller's explicit runnable-owner release makes capacity available.
	parentRelease()
	release, err := s.ReserveChild(t.Context(), request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := s.workflows[entryIdentity(request.Child)]; exists {
		t.Fatal("generated name registered in catalog")
	}
	if _, err = s.ReserveChild(t.Context(), request, time.Now()); err == nil {
		t.Fatal("same stable run admitted twice")
	}
	release()
	release()
	if s.conditions.ActiveWorkflow(request.Parent) != 0 {
		t.Fatal("child leaked parent bucket capacity")
	}
	if _, err = s.TriggerExact(t.Context(), entryIdentity(request.Child), time.Now()); err == nil {
		t.Fatal("generated name became a catalog trigger")
	}
}

func TestChildAdmissionConcurrentCallsCannotOverbookParent(t *testing.T) {
	parent, request := childAdmissionFixture()
	s, _ := newTestScheduler(t, []WorkflowEntry{parent})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var releases []func()
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidate := request
			candidate.RunID = fmt.Sprintf("%032x", i+10)
			release, err := s.ReserveChild(t.Context(), candidate, time.Now())
			if err == nil {
				mu.Lock()
				releases = append(releases, release)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(releases) != 1 || s.conditions.ActiveWorkflow(request.Parent) != 1 {
		t.Fatalf("overbooked: %d", len(releases))
	}
	releases[0]()
}

func TestChildAdmissionRestartRetainsCrashAttemptsAndIgnoresDriverEchoes(t *testing.T) {
	parent, request := childAdmissionFixture()
	collision := WorkflowEntry{Workflow: request.Child.Workflow, Gaggle: parent.Gaggle, Readiness: parent.Readiness}
	s, _ := newTestScheduler(t, []WorkflowEntry{parent, collision})
	now := time.Now()
	for range 2 {
		release, err := s.ReserveChild(t.Context(), request, now)
		if err != nil {
			t.Fatal(err)
		}
		// Simulates a process failure before the execution journal. Each successful
		// admission attempt retains its charge, although stable run custody is one.
		release()
	}
	if err := s.log.Append(journal.Event{Type: journal.EventRunStarted, Gaggle: parent.Gaggle, Workflow: request.Child.Workflow, RunID: request.RunID, Time: now}); err != nil {
		t.Fatal(err)
	}
	restored := New([]WorkflowEntry{parent, collision}, s.log)
	if err := restored.Reconcile(t.TempDir(), now); err != nil {
		t.Fatal(err)
	}
	next := request
	next.RunID = strings.Repeat("c", 32)
	_, err := restored.ReserveChild(t.Context(), next, now)
	requireChildRefusal(t, err, ReasonBudget)
	if got := len(restored.conditions.starts[entryIdentity(collision)]); got != 0 {
		t.Fatalf("driver echo charged name collision: %d", got)
	}
	if got := len(restored.conditions.starts[request.Parent]); got != 2 {
		t.Fatalf("restored child charges=%d", got)
	}
}

func TestChildAdmissionRecoveryCountsAndReleasesParentBucketByChildName(t *testing.T) {
	parent, request := childAdmissionFixture()
	runs := t.TempDir()
	digest := journal.Digest([]byte("pin"))
	lineage := journal.ChildLineage{Gaggle: parent.Gaggle, ParentRunID: request.ParentRunID, ParentWorkflow: parent.Workflow, StageOccurrence: "stage/0/1", InvocationKey: "child", AcceptanceID: "trigger-" + request.RunID, SourceDigest: digest, EnvelopeDigest: digest}
	run, err := journal.Create(runs, journal.RunIdentity{RunID: request.RunID, Gaggle: parent.Gaggle, Workflow: request.Child.Workflow, WorkflowVersion: 1, ConfigGeneration: digest, WorkflowDigest: digest, GooberDigest: digest, Child: &lineage, Trigger: journal.Trigger{Kind: journal.TriggerManual}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	s, _ := newTestScheduler(t, []WorkflowEntry{parent})
	if err = s.ReconcileRunDirs([]string{runs}, []string{filepath.Join(runs, request.RunID)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s.conditions.ActiveWorkflow(request.Parent) != 1 || s.conditions.ActiveWorkflow(entryIdentity(request.Child)) != 0 {
		t.Fatal("restart lost parent charge")
	}
	s.ReleaseRun(request.RunID, request.Child.Workflow)
	if s.conditions.ActiveWorkflow(request.Parent) != 0 {
		t.Fatal("child display name failed to release parent bucket")
	}
	s.ReleaseReconciled(request.RunID, request.Child.Workflow)
	if s.conditions.ActiveWorkflow(request.Parent) != 0 {
		t.Fatal("recovery release double-freed capacity")
	}
}

func TestChildAdmissionRefusesUnavailableScopeAndDurability(t *testing.T) {
	for _, mode := range []string{"gaggle", "provider", "unknown-parent", "disabled", "capability", "missing-log", "closed-log"} {
		t.Run(mode, func(t *testing.T) {
			parent, request := childAdmissionFixture()
			s, _ := newTestScheduler(t, []WorkflowEntry{parent})
			switch mode {
			case "gaggle":
				request.Child.Gaggle = "other"
			case "provider":
				request.Child.RepoRef.Provider = apiv1.ProviderADO
			case "unknown-parent":
				request.Parent.Workflow = "missing"
			case "disabled":
				parent.DisabledReason = "disabled"
				s.workflows[request.Parent] = parent
			case "capability":
				request.Child.RequiredCapabilities = []string{"unknown-tool@1"}
			case "missing-log":
				s.log = nil
			case "closed-log":
				if err := s.log.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.ReserveChild(t.Context(), request, time.Now()); err == nil {
				t.Fatal("unsafe child admitted")
			}
			if s.conditions.ActiveWorkflow(request.Parent) != 0 {
				t.Fatal("refusal retained capacity")
			}
		})
	}
}
