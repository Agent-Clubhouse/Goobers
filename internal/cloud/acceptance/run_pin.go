package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/startfence"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ErrRunPinsDiffer means a start confirmation carried different pins than the
// ones already recorded for that run.
var ErrRunPinsDiffer = errors.New("cloud acceptance: run pins differ from the recorded start")

// RunPins are a started engine run's immutable identity as the start fence
// resolved it: the RunInput the Temporal workflow runs, identified by its
// start-owner input digest. Control-side checks compare the run identity the
// worker journals against these, never the other way round.
type RunPins struct {
	RunID, AcceptanceID          string
	Gaggle, Workflow             string
	WorkflowVersion              int
	WorkflowDigest, GooberDigest string
	ConfigGeneration             string
	RepoRef                      apiv1.RepoRef
	BranchNamespace, InputDigest string
}

// RunPinsFor derives the pins of the start in.
func RunPinsFor(in engine.RunInput) (RunPins, error) {
	digest, err := startfence.InputDigest(in)
	if err != nil {
		return RunPins{}, err
	}
	pins := RunPins{RunID: in.RunID, AcceptanceID: in.AcceptanceID, Gaggle: in.Gaggle, Workflow: in.WorkflowName, WorkflowVersion: in.Version,
		WorkflowDigest: in.WorkflowDigest, GooberDigest: in.GooberDigest, ConfigGeneration: in.ConfigGeneration, RepoRef: in.RepoRef,
		BranchNamespace: in.BranchNamespace, InputDigest: digest}
	// Only the start identity is required. Pins are evidence for later
	// control-side checks, never a reason to refuse a start: a consumer that
	// needs a pin refuses its own operation when that pin is empty.
	if pins.RunID == "" || pins.AcceptanceID == "" {
		return RunPins{}, fmt.Errorf("%w: start input has no run identity", triggerqueue.ErrTransition)
	}
	return pins, nil
}

// recordRunPins writes pins once, inside the fenced transaction that confirms
// or adopts the start. A repeated confirmation of the same start is a no-op;
// different pins for a recorded run refuse the confirmation.
func (s *Store) recordRunPins(ctx context.Context, tx pgx.Tx, pins RunPins, epoch int64) error {
	repo, err := json.Marshal(pins.RepoRef)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO control.run_pin (instance_id, run_id, acceptance_id, gaggle, workflow, workflow_version, workflow_digest,
		  goober_digest, config_generation, repo_ref, branch_namespace, input_digest, owner_epoch)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (instance_id, run_id) DO NOTHING`,
		s.instanceID, pins.RunID, pins.AcceptanceID, pins.Gaggle, pins.Workflow, pins.WorkflowVersion, pins.WorkflowDigest,
		pins.GooberDigest, pins.ConfigGeneration, repo, pins.BranchNamespace, pins.InputDigest, epoch); err != nil {
		return err
	}
	var recorded RunPins
	var recordedRepo []byte
	err = tx.QueryRow(ctx, `
		SELECT run_id, acceptance_id, gaggle, workflow, workflow_version, workflow_digest, goober_digest, config_generation,
		  repo_ref, branch_namespace, input_digest
		FROM control.run_pin WHERE instance_id=$1 AND run_id=$2`, s.instanceID, pins.RunID,
	).Scan(&recorded.RunID, &recorded.AcceptanceID, &recorded.Gaggle, &recorded.Workflow, &recorded.WorkflowVersion, &recorded.WorkflowDigest,
		&recorded.GooberDigest, &recorded.ConfigGeneration, &recordedRepo, &recorded.BranchNamespace, &recorded.InputDigest)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(recordedRepo, &recorded.RepoRef); err != nil {
		return err
	}
	if recorded != pins {
		return ErrRunPinsDiffer
	}
	return nil
}
