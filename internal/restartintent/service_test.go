package restartintent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func fixture(t *testing.T) (*Service, runner.StageRestartPlan) {
	t.Helper()
	q, err := triggerqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	plan := runner.StageRestartPlan{Source: journal.RunIdentity{RunID: "source", Gaggle: "g", Workflow: "workflow", WorkflowDigest: journal.Digest([]byte("workflow")), ConfigGeneration: journal.Digest([]byte("generation"))}, GuidanceDigest: journal.Digest([]byte("guidance"))}
	m := map[string]any{"version": 1, "epochId": "epoch", "sourceRunId": "source", "sourceTerminalSeq": 7, "workflowDigest": plan.Source.WorkflowDigest, "stage": "work", "actor": "issuer:human", "guidance": "guidance", "guidanceIds": []string{"note"}, "guidanceDigest": plan.GuidanceDigest, "rationale": "reviewed"}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	plan.Continuation = journal.ContinuationRequest{RunID: "epoch", SourceRunID: "source", ExpectedTerminalSeq: 7, Operator: "issuer:human", Target: "work", Inputs: map[string][]byte{runner.StageRestartInputName: raw}, InputIntegrity: map[string]apiv1.Integrity{runner.StageRestartInputName: apiv1.IntegrityTrusted}, InputSource: map[string]string{runner.StageRestartInputName: "issuer:human"}}
	return &Service{Queue: q, Now: time.Now}, plan
}

func TestAcceptancePrecedesCapacityAndTransfersCustody(t *testing.T) {
	s, plan := fixture(t)
	record, _, err := s.Accept(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	count, err := s.Queue.PendingWorkflowStarts(t.Context(), triggerqueue.WorkflowPendingLimit{Gaggle: plan.Source.Gaggle, Workflow: plan.Source.Workflow})
	if err != nil || count != 1 {
		t.Fatal("queued restart missing from worker occupancy", count, err)
	}
	count, err = s.Queue.PendingWorkflowStarts(t.Context(), triggerqueue.WorkflowPendingLimit{Gaggle: plan.Source.Gaggle, Workflow: plan.Source.Workflow, ActiveRunIDs: []string{plan.Continuation.RunID}})
	if err != nil || count != 0 {
		t.Fatal("live restart counted twice", count, err)
	}
	published := false
	attempts := 0
	capacity := false
	s.Observe = func(context.Context, runner.StageRestartPlan) (bool, error) { return published, nil }
	s.Launch = func(ctx, _ context.Context, p runner.StageRestartPlan, before func(context.Context) error) error {
		attempts++
		if !capacity {
			return errors.New("capacity exhausted")
		}
		if err := before(ctx); err != nil {
			return err
		}
		published = true
		return nil
	}
	if err = s.Dispatch(t.Context(), t.Context(), record); err == nil {
		t.Fatal("capacity accepted")
	}
	still, err := s.Queue.ByKey(t.Context(), record.Key)
	if err != nil || still.State != triggerqueue.Accepted {
		t.Fatalf("lost queued custody: %+v %v", still, err)
	}
	pins, err := s.Retained(t.Context())
	if err != nil || !pins.Generations[plan.Source.ConfigGeneration] || !pins.Runs["g"]["source"] {
		t.Fatalf("lost pins: %+v %v", pins, err)
	}
	capacity = true
	if err = s.Dispatch(t.Context(), t.Context(), still); err != nil {
		t.Fatal(err)
	}
	done, err := s.Queue.ByKey(t.Context(), record.Key)
	if err != nil || done.State != triggerqueue.Dispatched || done.RunID != "epoch" {
		t.Fatalf("bad final receipt %+v %v", done, err)
	}
	if err = s.Dispatch(t.Context(), t.Context(), done); err != nil || attempts != 2 {
		t.Fatal("replayed execution", err, attempts)
	}
	pins, err = s.Retained(t.Context())
	if err != nil || len(pins.Generations) != 0 {
		t.Fatalf("custody not transferred: %+v %v", pins, err)
	}
}

func TestUncertainPublicationNeverUsesAbsentJournalAsResendPermission(t *testing.T) {
	s, plan := fixture(t)
	record, _, err := s.Accept(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	launched := 0
	published := false
	s.Observe = func(context.Context, runner.StageRestartPlan) (bool, error) { return published, nil }
	s.Launch = func(ctx, _ context.Context, _ runner.StageRestartPlan, before func(context.Context) error) error {
		launched++
		if err := before(ctx); err != nil {
			return err
		}
		return errors.New("lost reply after barrier")
	}
	if err = s.Dispatch(t.Context(), t.Context(), record); err == nil {
		t.Fatal("lost reply ignored")
	}
	uncertain, err := s.Queue.ByKey(t.Context(), record.Key)
	if err != nil || uncertain.State != triggerqueue.Dispatching {
		t.Fatal(uncertain, err)
	}
	for range 2 {
		if err = s.Dispatch(t.Context(), t.Context(), uncertain); err != nil {
			t.Fatal(err)
		}
	}
	if launched != 1 {
		t.Fatal("blind retry")
	}
	published = true
	if err = s.Reconcile(t.Context(), uncertain); err != nil {
		t.Fatal(err)
	}
	done, _ := s.Queue.ByKey(t.Context(), record.Key)
	if done.State != triggerqueue.Dispatched || done.RunID != plan.Continuation.RunID {
		t.Fatal(done)
	}
}

func TestExactPlanConflictAndOneEpochPerOccurrence(t *testing.T) {
	s, plan := fixture(t)
	record, _, err := s.Accept(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	again, repeated, err := s.Accept(t.Context(), plan)
	if err != nil || !repeated || again.ID != record.ID {
		t.Fatal(again, repeated, err)
	}
	changed := plan
	changed.Source.ConfigGeneration = "changed"
	if _, _, err = s.Accept(t.Context(), changed); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatal("changed pin accepted", err)
	}
	raw, err := runner.MarshalStageRestartPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	changed, err = runner.ParseStageRestartPlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	changed.Continuation.RunID = "another-epoch"
	var m map[string]any
	_ = json.Unmarshal(changed.Continuation.Inputs[runner.StageRestartInputName], &m)
	m["epochId"] = "another-epoch"
	changed.Continuation.Inputs[runner.StageRestartInputName], _ = json.Marshal(m)
	if _, _, err = s.Accept(t.Context(), changed); !errors.Is(err, triggerqueue.ErrConflict) {
		t.Fatal("forked same source occurrence", err)
	}

}

func TestQueueReopenRetainsPlanAndAuthority(t *testing.T) {
	s, plan := fixture(t)
	path := filepath.Join(t.TempDir(), "reopened.db")
	q, err := triggerqueue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Queue = q
	record, _, err := s.Accept(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Close(); err != nil {
		t.Fatal(err)
	}
	q, err = triggerqueue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close() }()
	s.Queue = q
	kept, err := s.Load(t.Context(), record)
	if err != nil || kept.Continuation.RunID != plan.Continuation.RunID || kept.GuidanceDigest != plan.GuidanceDigest {
		t.Fatal(kept, err)
	}
}
