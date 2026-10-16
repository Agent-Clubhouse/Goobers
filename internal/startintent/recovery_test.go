package startintent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type blockedStarter struct {
	entered chan struct{}
	finish  chan struct{}
}

func (s *blockedStarter) Start(context.Context, localscheduler.StartRequest) (localscheduler.StartResult, error) {
	close(s.entered)
	<-s.finish
	return localscheduler.StartResult{Phase: journal.PhaseCompleted}, nil
}

func dispatchFixture(t *testing.T) (*Service, *blockedStarter, *localscheduler.Scheduler, *atomic.Int32) {
	t.Helper()
	s := intentService(t)
	log, _, err := journal.OpenInstanceLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	starter := &blockedStarter{entered: make(chan struct{}), finish: make(chan struct{})}
	entry := localscheduler.WorkflowEntry{Workflow: "repair", Gaggle: "own", Starter: starter}
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{entry}, log)
	var released atomic.Int32
	s.Build = func(context.Context, Target) (Prepared, error) {
		return Prepared{Entry: entry, Release: func() { released.Add(1) }}, nil
	}
	s.Scheduler = func() *localscheduler.Scheduler { return scheduler }
	s.RunDirectory = func(context.Context, string) (string, error) { return "", nil }
	t.Cleanup(func() { close(starter.finish); scheduler.Wait() })
	return s, starter, scheduler, &released
}

func TestDispatchRetainsUncertaintyAndLeaseUntilStarterReturns(t *testing.T) {
	s, starter, scheduler, released := dispatchFixture(t)
	record, _, err := s.Accept(t.Context(), "delayed-publication", "alice", Request{Workflow: "repair"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(t.Context(), t.Context(), record); err != nil {
		t.Fatal(err)
	}
	<-starter.entered
	stored, err := s.Queue.Get(t.Context(), record.ID, "alice")
	if err != nil || stored.State != triggerqueue.Dispatching || stored.RunID != strings.TrimPrefix(record.ID, "trigger-") {
		t.Fatal(stored, err)
	}
	if released.Load() != 0 {
		t.Fatal("released archive while starter owns execution")
	}
	if observed, err := s.Observe(t.Context(), stored); err != nil || observed {
		t.Fatal(observed, err)
	}
	// Absence during a live async launch must retain custody, not report success.
	pins, err := RetainedGenerations(t.Context(), s.Queue)
	if err != nil || !pins["generation-1"] {
		t.Fatal(pins, err)
	}
	starter.finish <- struct{}{}
	scheduler.Wait()
	if released.Load() != 1 {
		t.Fatal("execution lease not released exactly once", released.Load())
	}
}

func TestAcceptedJournalRecoveryDoesNotLaunchAgain(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			s, starter, _, released := dispatchFixture(t)
			record, _, err := s.Accept(t.Context(), "published", "alice", Request{Workflow: "repair"})
			if err != nil {
				t.Fatal(err)
			}
			runID := strings.TrimPrefix(record.ID, "trigger-")
			id := journal.RunIdentity{RunID: runID, Workflow: "repair", Gaggle: "own", WorkflowVersion: 1, ConfigGeneration: "generation-1", WorkflowDigest: "workflow-1", GooberDigest: "goober-1", Trigger: journal.Trigger{Kind: journal.TriggerManual, Ref: "repair"}}
			if mismatch {
				id.ConfigGeneration = "different-generation"
			}
			root := t.TempDir()
			run, err := journal.Create(root, id, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			s.RunDirectory = func(context.Context, string) (string, error) { return filepath.Join(root, runID), nil }
			err = s.Dispatch(t.Context(), t.Context(), record)
			if (err != nil) != mismatch {
				t.Fatal("journal comparison", err)
			}
			stored, err := s.Queue.Get(t.Context(), record.ID, "alice")
			if err != nil {
				t.Fatal(err)
			}
			want := triggerqueue.Dispatched
			if mismatch {
				want = triggerqueue.Accepted
			}
			if stored.State != want {
				t.Fatal(stored)
			}
			select {
			case <-starter.entered:
				t.Fatal("launched despite preexisting journal")
			default:
			}
			if released.Load() != 1 {
				t.Fatal("recovery leaked lease")
			}
		})
	}
}

func TestRetentionPagesAllUnfinishedStartsAndDropsFinishedPins(t *testing.T) {
	s := intentService(t)
	var records []triggerqueue.Record
	for i := range 205 {
		generation := fmt.Sprintf("generation-%03d", i)
		s.Capture = func(context.Context, Request) (Target, func(), error) {
			return Target{Gaggle: "own", Workflow: "repair", ConfigGeneration: generation, WorkflowDigest: "workflow", GooberDigest: "goober"}, func() {}, nil
		}
		record, _, err := s.Accept(t.Context(), generation, "alice", Request{Workflow: "repair"})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := s.Queue.BeginDispatch(t.Context(), records[101].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Queue.BeginDispatch(t.Context(), records[204].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Queue.Finish(t.Context(), records[204].ID, triggerqueue.Rejected, "", "disabled", time.Now()); err != nil {
		t.Fatal(err)
	}
	pins, err := RetainedGenerations(t.Context(), s.Queue)
	if err != nil || len(pins) != 204 || !pins["generation-101"] || pins["generation-204"] {
		t.Fatal(len(pins), err)
	}
	if _, _, err := s.Queue.Accept(t.Context(), "legacy", "alice", []byte(`{"workflow":"legacy"}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if pins, err := RetainedGenerations(t.Context(), s.Queue); err != nil || len(pins) != 204 {
		t.Fatal(pins, err)
	}
	if _, _, err := s.Queue.Accept(t.Context(), "corrupt", "alice", []byte(`{"kind":"goobers.workflow-start/v1"}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := RetainedGenerations(t.Context(), s.Queue); err == nil {
		t.Fatal("corrupt custody allowed pruning")
	}
}
