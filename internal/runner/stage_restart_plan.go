package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// MaxStageRestartPlanBytes bounds the complete durable launch snapshot,
// including retained inputs and selected guidance. It shares the queue limit.
const MaxStageRestartPlanBytes = 4 << 20

type retainedStageRestartPlan struct {
	Version int              `json:"version"`
	Plan    StageRestartPlan `json:"plan"`
}

// MarshalStageRestartPlan snapshots trusted preparation before child epoch
// admission. Runtime callbacks cannot be serialized. The admitted child lineage
// is attached only after the queue returns its request digest, avoiding a
// self-referential digest and preventing persisted plans from granting custody.
func MarshalStageRestartPlan(plan StageRestartPlan) ([]byte, error) {
	if err := validateRetainedRestartPlan(plan); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(retainedStageRestartPlan{Version: 1, Plan: plan})
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxStageRestartPlanBytes {
		return nil, errors.New("runner: retained restart plan exceeds 4 MiB")
	}
	return raw, nil
}

// ParseStageRestartPlan restores exact bounded preparation, never admission.
// The caller must still verify its queue digest, source terminal generation,
// current human authority and parent fence before publishing or resuming it.
func ParseStageRestartPlan(raw []byte) (StageRestartPlan, error) {
	if len(raw) == 0 || len(raw) > MaxStageRestartPlanBytes {
		return StageRestartPlan{}, errors.New("runner: invalid retained restart plan size")
	}
	var retained retainedStageRestartPlan
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&retained); err != nil {
		return StageRestartPlan{}, err
	}
	if retained.Version != 1 {
		return StageRestartPlan{}, errors.New("runner: unsupported retained restart plan version")
	}
	canonical, err := MarshalStageRestartPlan(retained.Plan)
	if err != nil || !bytes.Equal(canonical, raw) {
		return StageRestartPlan{}, errors.New("runner: retained restart plan is not canonical or valid")
	}
	return retained.Plan, nil
}

func validateRetainedRestartPlan(plan StageRestartPlan) error {
	req, source := plan.Continuation, plan.Source
	if err := validateRetainedRestartIdentity(plan); err != nil {
		return err
	}
	raw, ok := req.Inputs[StageRestartInputName]
	if !ok || req.InputIntegrity[StageRestartInputName] != apiv1.IntegrityTrusted || req.InputSource[StageRestartInputName] != req.Operator {
		return errors.New("runner: retained restart plan lacks trusted guidance snapshot")
	}
	var m stageRestartManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if m.Version != 1 || m.EpochID != req.RunID || m.SourceRunID != source.RunID || m.SourceTerminalSeq != req.ExpectedTerminalSeq || m.WorkflowDigest != source.WorkflowDigest || m.Stage != req.Target || m.Actor != req.Operator || m.GuidanceDigest != plan.GuidanceDigest || m.GuidanceDigest != journal.Digest([]byte(m.Guidance)) {
		return errors.New("runner: retained restart manifest differs from its plan")
	}
	return nil
}

func validateRetainedRestartIdentity(plan StageRestartPlan) error {
	req, source := plan.Continuation, plan.Source
	if req.VerifySourceBranch != nil || req.ChildContinuation != nil || req.ChildWorkspace != nil || !apiv1.ValidRunID(req.RunID) || req.RunID == source.RunID || req.SourceRunID != source.RunID || req.ExpectedTerminalSeq == 0 || req.Target == "" || req.Operator == "" {
		return errors.New("runner: retained restart plan has unbound identity or runtime admission")
	}
	if source.Child != nil && source.ValidateChildLineage() != nil {
		return errors.New("runner: retained restart source has invalid child lineage")
	}
	return nil
}

// MatchesStageRestartRequest checks an exact human command against its accepted
// snapshot without rereading changed source history or selected guidance.
func MatchesStageRestartRequest(plan StageRestartPlan, request StageRestartRequest) bool {
	var manifest stageRestartManifest
	if json.Unmarshal(plan.Continuation.Inputs[StageRestartInputName], &manifest) != nil {
		return false
	}
	return manifest.EpochID == request.EpochID && manifest.Stage == request.Stage && manifest.Actor == request.PrincipalRef && manifest.SourceTerminalSeq == request.ExpectedTerminalSeq && manifest.Rationale == request.Rationale && slices.Equal(manifest.GuidanceIDs, request.GuidanceIDs)
}

// RetainedStageRestartRequest extracts the exact accepted command for compact
// no-effect replay. It grants no execution or source custody.
func RetainedStageRestartRequest(plan StageRestartPlan) (StageRestartRequest, error) {
	var manifest stageRestartManifest
	if err := json.Unmarshal(plan.Continuation.Inputs[StageRestartInputName], &manifest); err != nil {
		return StageRestartRequest{}, err
	}
	request := StageRestartRequest{EpochID: manifest.EpochID, Stage: manifest.Stage, PrincipalRef: manifest.Actor, ExpectedTerminalSeq: manifest.SourceTerminalSeq, GuidanceIDs: slices.Clone(manifest.GuidanceIDs), Rationale: manifest.Rationale}
	if !MatchesStageRestartRequest(plan, request) || manifest.EpochID != plan.Continuation.RunID || manifest.SourceRunID != plan.Source.RunID {
		return StageRestartRequest{}, errors.New("runner: retained restart command differs")
	}
	return request, nil
}
