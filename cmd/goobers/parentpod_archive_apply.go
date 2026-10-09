package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
)

const parentRestorePreparedKind = "isolated.parent.restore.prepared"
const maxParentRestorePlanBytes = 2 << 20

type parentRestorePlan struct {
	Version       int                     `json:"version"`
	RetirementSeq uint64                  `json:"retirementSeq"`
	Archive       journal.Ref             `json:"archive"`
	Plan          recovery.ChildApplyPlan `json:"plan"`
}

type parentRestorePrepared struct {
	RetirementSeq uint64      `json:"retirementSeq"`
	Plan          journal.Ref `json:"plan"`
}

func applyParentArchiveRestore(ctx context.Context, rec runner.OwnedJournalRecorder, reader *journal.Reader, repository string, archive runner.ParentWorkspaceArchive, seq uint64, record recovery.Record, maxBytes int64) error {
	_, branch, err := runner.OwnedJournalScope(rec)
	if err != nil {
		return err
	}
	plan, found, err := readParentRestorePlan(reader, branch, archive.Archive, seq)
	if err != nil {
		return err
	}
	if !found {
		// Retirement may be durable while removal never happened. A surviving
		// exact checkout needs no file mutation and therefore no apply intent.
		if err := recovery.VerifyRetainedParentCheckout(ctx, repository, record, maxBytes); err == nil {
			return nil
		} else if !errors.Is(err, recovery.ErrWorkspaceChanged) {
			return err
		}
		application, err := recovery.PlanRetainedParentRestore(ctx, repository, record, fmt.Sprintf("parent-restore-%d", seq), maxBytes)
		if err != nil {
			return err
		}
		plan = parentRestorePlan{Version: 1, RetirementSeq: seq, Archive: archive.Archive, Plan: application}
		if err := recordParentRestorePlan(rec, plan); err != nil {
			return err
		}
	}
	if err := recovery.ApplyChildApplication(ctx, repository, plan.Plan); err != nil {
		return err
	}
	return recovery.VerifyRetainedParentCheckout(ctx, repository, record, maxBytes)
}

func recordParentRestorePlan(rec runner.OwnedJournalRecorder, plan parentRestorePlan) error {
	data, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	ref, err := rec.RecordArtifactBoundedWithIntegrity(fmt.Sprintf("parent-restore-%d.json", plan.RetirementSeq), data, apiv1.IntegrityTrusted, maxParentRestorePlanBytes)
	if err != nil {
		return err
	}
	if ref.Digest != journal.Digest(data) {
		return errors.New("parent restore intent changed during recording")
	}
	return rec.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": parentRestorePreparedKind, "restore": parentRestorePrepared{RetirementSeq: plan.RetirementSeq, Plan: ref}}})
}

func readParentRestorePlan(reader *journal.Reader, branch int, archive journal.Ref, seq uint64) (parentRestorePlan, bool, error) {
	var result parentRestorePlan
	events, err := reader.Events()
	if err != nil {
		return result, false, err
	}
	found := false
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != parentRestorePreparedKind {
			continue
		}
		var prepared parentRestorePrepared
		data, err := json.Marshal(event.Runner["restore"])
		if err != nil || len(data) > 4096 || json.Unmarshal(data, &prepared) != nil {
			return result, false, errors.New("invalid parent restore intent receipt")
		}
		if prepared.RetirementSeq != seq {
			continue
		}
		if found || event.Branch != branch || event.Seq <= seq || prepared.Plan.Integrity != apiv1.IntegrityTrusted {
			return result, false, errors.New("parent restore intent ownership changed")
		}
		data, err = reader.ArtifactBytesBounded(prepared.Plan, maxParentRestorePlanBytes)
		if err != nil {
			return result, false, err
		}
		if json.Unmarshal(data, &result) != nil || result.Version != 1 || result.RetirementSeq != seq || result.Archive != archive {
			return result, false, errors.New("parent restore intent differs from retirement")
		}
		found = true
	}
	return result, found, nil
}
