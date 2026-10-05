package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/restartintent"
	"github.com/goobers/goobers/internal/telemetry/retention"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func cancelledHumanRestart(t *testing.T) (*humanAcceptanceFixture, *upSession, apicontract.InteractiveRunCommand, triggerqueue.Record) {
	t.Helper()
	f := newHumanAcceptanceFixture(t)
	command := f.restartCommand(t)
	accepted := f.command(t, "cancelled-command", command)
	c := f.setup.Definitions.Gaggles[0].DeepCopy()
	c.Spec.InteractiveAccess.Actions = append(c.Spec.InteractiveAccess.Actions, "queue.cancel")
	if err := f.setup.InteractiveAccess.Apply([]apiv1.Gaggle{*c}, nil); err != nil {
		t.Fatal(err)
	}
	f.durable.startControls = newStartQueueControls(f.pinned.layout, f.durable.queue, time.Now)
	record, err := f.durable.queue.ByKey(t.Context(), restartintent.Key(accepted.ContinuationRunID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.durable.startControls.Ensure(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	u := &upSession{}
	u.l, u.setup, u.durableTriggers = f.pinned.layout, f.setup, f.durable
	u.configureStartQueue()
	p := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
	opts := append(u.apiHandlerOpts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &p}))
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), opts...)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/v1/gaggles/example/start-queue/"+record.ID+"/cancel", strings.NewReader(`{"requestId":"cancel","reason":"Revise instructions"}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	var receipt apicontract.StartQueueItem
	if err = json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || response.Code != 202 || receipt.Disposition != "cancelled" {
		t.Fatal(response.Code, response.Body, err)
	}
	return f, u, command, record
}
func TestHumanRestartFreshCommandAfterCancellationAndCompactReplay(t *testing.T) {
	f, _, command, old := cancelledHumanRestart(t)
	receipt := f.command(t, "cancelled-command", command)
	if receipt.Status != "cancelled" || receipt.ContinuationRunID != "" {
		t.Fatal(receipt)
	}
	service := &restartintent.Service{Queue: f.durable.queue, Now: func() time.Time { return time.Now().Add(triggerqueue.ReplayRetention + time.Hour) }}
	if err := service.CompactRejected(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	replay, err := f.durable.queue.HumanRestartReplay(t.Context(), old.ID)
	if err != nil || len(replay.Replay) == 0 {
		t.Fatal(replay, err)
	}
	if _, _, err = f.durable.queue.Accept(t.Context(), "maintenance", "host", []byte(`{}`), service.Now()); err != nil {
		t.Fatal(err)
	}
	receipt = f.command(t, "cancelled-command", command)
	if receipt.Status != "cancelled" || receipt.ContinuationRunID != "" {
		t.Fatal("old key revived", receipt)
	}
	saved := f.command(t, "fresh-guidance", apicontract.InteractiveRunCommand{Kind: "guidance", Stage: command.Stage, ExpectedSubjectSequence: command.ExpectedSubjectSequence, Guidance: "New instructions reviewed after cancellation"})
	command.GuidanceIDs = []string{saved.Guidance.Request.RequestID}
	command.Rationale = "Explicit new review"
	fresh := f.command(t, "fresh-command", command)
	if fresh.Status != "pending" || fresh.ContinuationRunID == replay.Epoch {
		t.Fatal(fresh)
	}
	if err = f.durable.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wg.Wait()
	fresh = f.command(t, "fresh-command", command)
	if fresh.Status != "started" {
		t.Fatal(fresh)
	}
	if _, err = f.pinned.layout.FindRunDir(replay.Epoch); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled epoch executed", err)
	}
	f.process.mu.Lock()
	count := len(f.process.requests)
	f.process.mu.Unlock()
	if count != 2 {
		t.Fatal("fresh retry allowance or execution duplicated", count)
	}
}
func TestRestartTombstoneWaitsForMarkedSourceRetirementAndMaintenanceExclusion(t *testing.T) {
	f, u, _, old := cancelledHumanRestart(t)
	future := time.Now().Add(triggerqueue.ReplayRetention + time.Hour)
	service := &restartintent.Service{Queue: f.durable.queue, Now: func() time.Time { return future }}
	if err := service.CompactRejected(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	r, err := f.durable.queue.HumanRestartReplay(t.Context(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.durable.queue.ForgetRetiredHumanRestart(t.Context(), old.ID, r.Gaggle, r.SourceRun); !errors.Is(err, triggerqueue.ErrTransition) {
		t.Fatal("unmarked source erased key", err)
	}
	sourceDir, err := f.pinned.layout.FindRunDir(r.SourceRun)
	if err != nil {
		t.Fatal(err)
	}
	if err = markRestartSourcePruning(t.Context(), f.durable.queue, retention.Result{RunID: r.SourceRun, RunDir: sourceDir}, future); err != nil {
		t.Fatal(err)
	}
	r, err = f.durable.queue.HumanRestartReplay(t.Context(), old.ID)
	if err != nil || !r.Retiring {
		t.Fatal(r, err)
	}
	if err = u.forgetRetiredRestart(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if _, err = f.durable.queue.ByKey(t.Context(), old.Key); err != nil {
		t.Fatal("live source lost key", err)
	}
	staged := filepath.Join(filepath.Dir(filepath.Dir(sourceDir)), ".telemetry-pruning", r.SourceRun)
	if err = os.MkdirAll(filepath.Dir(staged), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(sourceDir, staged); err != nil {
		t.Fatal(err)
	}
	if err = u.forgetRetiredRestart(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if _, err = f.durable.queue.ByKey(t.Context(), old.Key); err != nil {
		t.Fatal("staged source lost key", err)
	}
	if err = os.Rename(staged, sourceDir); err != nil {
		t.Fatal(err)
	}
	if err = u.forgetRetiredRestart(t.Context(), r); err != nil {
		t.Fatal("rollback failed", err)
	}
	savedDir := filepath.Join(t.TempDir(), "retained-source")
	if err = os.Rename(sourceDir, savedDir); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(sourceDir, []byte("unreadable journal location"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = u.forgetRetiredRestart(t.Context(), r); err == nil {
		t.Fatal("invalid source path became absence proof")
	}
	if _, err = f.durable.queue.ByKey(t.Context(), old.Key); err != nil {
		t.Fatal("invalid inventory released key", err)
	}
	if err = os.Remove(sourceDir); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(savedDir, sourceDir); err != nil {
		t.Fatal(err)
	}
	roots, err := f.pinned.layout.RunDirsContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	locks, err := journal.AcquireRunRootMaintenanceLocks(roots)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(sourceDir); err != nil {
		t.Fatal(err)
	}
	if err = u.forgetRetiredRestart(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if _, err = f.durable.queue.ByKey(t.Context(), old.Key); err != nil {
		t.Fatal("contended retirement released key", err)
	}
	if err = locks.Release(); err != nil {
		t.Fatal(err)
	}
	if err = u.forgetRetiredRestart(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if _, err = f.durable.queue.ByKey(t.Context(), old.Key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("retired key not pruned", err)
	}
}
