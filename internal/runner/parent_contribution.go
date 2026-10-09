package runner

import (
	"encoding/json"
	"errors"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
)

// ParentContributionKind is a host-only receipt for a verified imported tree.
const ParentContributionKind = "isolated.parent.contribution"

// ParentContributionRetiredKind records a durable retirement authorization,
// after archive verification and before checkout removal. Cleanup is idempotent;
// the referenced output remains readable under ordinary journal retention.
const ParentContributionRetiredKind = "isolated.parent.contribution.retired"

const maxParentContributionBytes = 24 << 20

type parentContribution struct {
	Version        int                             `json:"version"`
	ContractDigest string                          `json:"contractDigest"`
	Custody        ContainedParentWorkspaceCustody `json:"custody"`
	Output         journal.Ref                     `json:"output"`
}

// RecordParentContribution is called only after stopped pod output has been
// verified and imported. It binds the exact artifact ref to the held checkout.
func RecordParentContribution(rec OwnedJournalRecorder, env apiv1.InvocationEnvelope, contract string, output journal.Ref) error {
	_, branch, err := OwnedJournalScope(rec)
	if err != nil || env.ChildWorkflowOrigin == nil {
		return errors.Join(errors.New("parent contribution lacks owned origin"), err)
	}
	if _, err := ParentWorkspaceCustody(rec, env); err != nil {
		return err
	}
	reader, err := journal.OpenReadOnly(rec.Dir())
	if err != nil {
		return err
	}
	if _, err := reader.ArtifactBytesBounded(output, maxParentContributionBytes); err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	started, custody, err := parentContributionCustody(events, env, branch)
	if err != nil {
		return err
	}
	if !blobstore.ValidDigest(contract) {
		return errors.New("invalid parent contribution contract")
	}
	record := parentContribution{Version: 1, ContractDigest: contract, Custody: custody, Output: output}
	exists, err := parentContributionReplay(events, record)
	if err != nil || exists {
		return err
	}
	return rec.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: started.Stage, Attempt: started.Attempt, Branch: branch, Runner: map[string]any{"kind": ParentContributionKind, "contractDigest": contract, "contribution": record}})
}

func parentContributionCustody(events []journal.Event, env apiv1.InvocationEnvelope, branch int) (journal.Event, ContainedParentWorkspaceCustody, error) {
	var custody ContainedParentWorkspaceCustody
	var started journal.Event
	for _, event := range events {
		if event.Branch != branch {
			continue
		}
		if event.Type == journal.EventStageStarted {
			if started.Seq != 0 && event.Stage == started.Stage {
				return started, custody, errors.New("parent contribution attempt was superseded")
			}
			origin, err := journal.ChildWorkflowOriginForEvent(env.RunID, event)
			if err == nil && *origin == *env.ChildWorkflowOrigin {
				started = event
				custody = ContainedParentWorkspaceCustody{}
			}
		}
		if started.Seq != 0 && event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == ContainedParentWorkspaceKind && event.Stage == started.Stage && event.Attempt == started.Attempt {
			if custody.Origin != nil {
				return started, custody, errors.New("duplicate parent workspace custody")
			}
			raw, _ := json.Marshal(event.Runner["custody"])
			if json.Unmarshal(raw, &custody) != nil {
				return started, custody, errors.New("invalid parent workspace custody")
			}
		}
	}
	if started.Seq == 0 || custody.Origin == nil || *custody.Origin != *env.ChildWorkflowOrigin || custody.Version != 1 || custody.Workspace.OwnerRunID != env.RunID || custody.Workspace.WorkspaceID == "" || custody.Workspace.RepositoryDigest == "" || custody.Workspace.Branch == "" || custody.Workspace.StartRef == "" {
		return started, custody, errors.New("parent contribution differs from held checkout")
	}
	return started, custody, nil
}

func parentContributionReplay(events []journal.Event, record parentContribution) (bool, error) {
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != ParentContributionKind || event.Runner["contractDigest"] != record.ContractDigest {
			continue
		}
		previous, err := decodeParentContribution(event)
		if err != nil || !reflect.DeepEqual(previous, record) {
			return false, errors.Join(errors.New("parent contribution changed on replay"), err)
		}
		return true, nil
	}
	return false, nil
}

func decodeParentContribution(event journal.Event) (parentContribution, error) {
	var result parentContribution
	raw, err := json.Marshal(event.Runner["contribution"])
	if err != nil || len(raw) > 8192 || json.Unmarshal(raw, &result) != nil || result.Version != 1 || !blobstore.ValidDigest(result.ContractDigest) || result.ContractDigest != event.Runner["contractDigest"] || result.Custody.Version != 1 || result.Custody.Origin == nil || result.Output.Path == "" || result.Output.Size <= 0 || result.Output.Size > maxParentContributionBytes {
		return result, errors.New("invalid parent contribution receipt")
	}
	return result, nil
}

// ParentWorkspaceCustody requires a host-held checkout before dispatch. It does
// not create or adopt a workspace; the runner still owns that exclusive lease.
func ParentWorkspaceCustody(rec OwnedJournalRecorder, env apiv1.InvocationEnvelope) (ContainedParentWorkspaceCustody, error) {
	var empty ContainedParentWorkspaceCustody
	owned, branch, err := OwnedJournalScope(rec)
	if err != nil || env.ChildWorkflowOrigin == nil {
		return empty, errors.Join(errors.New("parent managed workspace custody unavailable"), err)
	}
	reader, err := journal.OpenReadOnly(owned.Dir())
	if err != nil {
		return empty, err
	}
	id, err := reader.Identity()
	if err != nil {
		return empty, err
	}
	if id.Child != nil || id.RunID != env.RunID || id.InstanceID != env.InstanceID || id.Gaggle != env.Gaggle || id.Workflow != env.WorkflowID || id.ConfigGeneration != env.ConfigGeneration || id.GooberDigest != env.GooberDigest {
		return empty, errors.New("parent managed workspace identity changed")
	}
	events, err := reader.Events()
	if err != nil {
		return empty, err
	}
	_, custody, err := parentContributionCustody(events, env, branch)
	return custody, err
}
