package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestOrdinaryFileTransferRecoversLostAckAndStartsOnce(t *testing.T) {
	layout := instance.NewLayout(initDeterministicDemo(t))
	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(t.Context(), layout, &wg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := setup.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	sched := localscheduler.New(setup.Entries, setup.InstanceLog, setup.SchedulerOptions()...)
	dispatch := newDaemonTriggerService()
	dispatch.AttachScheduler(sched)
	dispatch.AttachDispatchContext(t.Context())
	queuePath := filepath.Join(layout.SchedulerDir(), "accepted-triggers.db")
	service := acceptedService(t, queuePath, dispatch)
	if err := setup.installOrdinaryStarts(layout, service); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sweep := func(recover bool) {
		t.Helper()
		if err := sweepPendingTriggersWithAdmission(t.Context(), layout.SchedulerDir(), nil, sched, func() time.Time { return now }, triggerSweepOptions{recoverActiveRequests: recover}, service.delegatedAdmission()); err != nil {
			t.Fatal(err)
		}
	}
	id, err := writeTriggerRequestContext(t.Context(), layout.SchedulerDir(), "example", "default-implement")
	if err != nil {
		t.Fatal(err)
	}
	sweep(false)
	first, err := service.queue.ByKey(t.Context(), "delegated:"+id)
	if err != nil || first.State != triggerqueue.Accepted {
		t.Fatal(first, err)
	}
	if withdrawn, err := withdrawTriggerRequest(layout.SchedulerDir(), id); err != nil || withdrawn {
		t.Fatal("false withdrawal", withdrawn, err)
	}
	dir := filepath.Join(layout.SchedulerDir(), pendingTriggersDir)
	if err := os.Remove(filepath.Join(dir, id+ackSuffix)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, id+requestSuffix), filepath.Join(dir, id+activeSuffix)); err != nil {
		t.Fatal(err)
	}
	if err := service.queue.Close(); err != nil {
		t.Fatal(err)
	}
	service = acceptedService(t, queuePath, dispatch)
	if err := setup.installOrdinaryStarts(layout, service); err != nil {
		t.Fatal(err)
	}
	// Old file deadlines cannot override accepted custody during recovery.
	now = now.Add(2 * time.Hour)
	sweep(true)
	retry, err := service.queue.ByKey(t.Context(), "delegated:"+id)
	if err != nil || retry.ID != first.ID || string(retry.Payload) != string(first.Payload) {
		t.Fatal(retry, err)
	}
	writeFileContent(t, filepath.Join(layout.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml"), "invalid replacement: [")
	if err := service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	sched.Wait()
	wg.Wait()
	if err := service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	sweep(false)
	response, ok := readTriggerResponseFile(filepath.Join(dir, id+responseSuffix))
	runID := strings.TrimPrefix(first.ID, "trigger-")
	if !ok || response.RunID != runID || response.Error != "" {
		t.Fatal(response, ok)
	}
	runDir, err := layout.FindRunDir(runID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(runDir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := startintent.VerifyIdentity(identity, first); err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil || journal.PhaseFromEvents(events) != journal.PhaseCompleted {
		t.Fatal(events, err)
	}
	if err := writeDelegateJSON(filepath.Join(dir, id+requestSuffix), triggerRequest{Workflow: "default-implement", Gaggle: "example", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	sweep(false)
	response, ok = readTriggerResponseFile(filepath.Join(dir, id+responseSuffix))
	if !ok || response.RunID != runID {
		t.Fatal(response, ok)
	}
	dirs, err := os.ReadDir(layout.ForGaggle("example").RunsDir())
	if err != nil || len(dirs) != 1 {
		t.Fatal("duplicate execution", len(dirs), err)
	}
}

func TestDelegatedReceiptWaitsForPublishedRun(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "test"+activeSuffix)
	if err := os.WriteFile(active, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	req := triggerRequest{QueueTransfer: true}
	record := triggerqueue.Record{ID: "receipt", State: triggerqueue.Dispatching, RunID: "reserved", AcceptedAt: time.Now()}
	if err := publishDelegatedReceipt(dir, "test", active, &req, record); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "test"+responseSuffix)); !os.IsNotExist(err) {
		t.Fatal("unpublished run reported as created", err)
	}
	if req.AcceptanceID != record.ID {
		t.Fatal(req)
	}
}

func TestDelegatedWithdrawalRetainsUnknownCustody(t *testing.T) {
	for _, body := range []string{`{"queueTransfer":true}`, `{"acceptanceId":"receipt"}`, `{broken`} {
		t.Run(body, func(t *testing.T) {
			dir := t.TempDir()
			requests := filepath.Join(dir, pendingTriggersDir)
			if err := os.MkdirAll(requests, 0700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(requests, "test"+requestSuffix)
			if err := os.WriteFile(file, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			withdrawn, _ := withdrawTriggerRequest(dir, "test")
			if withdrawn {
				t.Fatal("unknown custody reported withdrawn")
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDelegatedTransferPinsDeadlinesAndSelectors(t *testing.T) {
	for _, mode := range []string{"manual", "priority", "short-deadline", "targeted-pr"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			service := acceptedService(t, filepath.Join(t.TempDir(), "queue.db"), newDaemonTriggerService())
			now := time.Now().UTC()
			service.dispatch.now = func() time.Time { return now }
			service.ordinary = &startintent.Service{Queue: service.queue, Now: service.dispatch.now, Capture: func(context.Context, startintent.Request) (startintent.Target, func(), error) {
				return startintent.Target{Workflow: "work", Gaggle: "own", ConfigGeneration: "captured", WorkflowDigest: "workflow", GooberDigest: "goober"}, func() {}, nil
			}}
			req := triggerRequest{Workflow: "work", Gaggle: "own", CreatedAt: now}
			want := now.Add(acceptedTriggerQueueLifetime())
			switch mode {
			case "priority":
				req.Priority = true
				req.SourceRun = "source"
				want = now.Add(priorityTriggerTimeout)
			case "short-deadline":
				req.Deadline = now.Add(time.Second)
				want = req.Deadline
			case "targeted-pr":
				req.PR = 42
			}
			active := filepath.Join(dir, "test"+activeSuffix)
			if err := writeDelegateJSON(active, req); err != nil {
				t.Fatal(err)
			}
			handled, err := service.transferDelegated(t.Context(), dir, "test", active, &req, false)
			if err != nil || !handled {
				t.Fatal(handled, err)
			}
			first, err := service.queue.ByKey(t.Context(), "delegated:test")
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := startintent.Parse(first.Payload)
			if err != nil || !envelope.Deadline.Equal(want) || envelope.Request.PullRequest != req.PR || envelope.Request.SourceRun != req.SourceRun {
				t.Fatal(envelope, err)
			}
			// Replay an original file after acknowledgement loss, without its receipt.
			req.AcceptanceID = ""
			req.AcceptedAt = time.Time{}
			now = now.Add(time.Minute)
			if err := writeDelegateJSON(active, req); err != nil {
				t.Fatal(err)
			}
			service.ordinary.Capture = func(context.Context, startintent.Request) (startintent.Target, func(), error) {
				t.Fatal("recaptured accepted request")
				return startintent.Target{}, nil, nil
			}
			handled, err = service.transferDelegated(t.Context(), dir, "test", active, &req, true)
			if err != nil || !handled {
				t.Fatal(handled, err)
			}
			retry, err := service.queue.ByKey(t.Context(), "delegated:test")
			if err != nil || retry.ID != first.ID || string(retry.Payload) != string(first.Payload) {
				t.Fatal(retry, err)
			}
		})
	}
}
