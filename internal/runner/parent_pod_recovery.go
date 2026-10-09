package runner

import (
	"encoding/json"
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// ContainedParentRecoveredKind is host-only acknowledged workspace recovery.
// It transfers retained working changes into the next same-stage attempt.
const ContainedParentRecoveredKind = "isolated.parent.recovered"
const maxParentRecoveryBytes = 256 << 10

type containedParentRecovery struct {
	Version             int                             `json:"version"`
	ContractDigest      string                          `json:"contractDigest"`
	Custody             ContainedParentWorkspaceCustody `json:"custody"`
	Context             []apiv1.ContextPointer          `json:"context,omitempty"`
	Transcript          *apiv1.ArtifactPointer          `json:"transcript,omitempty"`
	Usage               map[string]float64              `json:"usage,omitempty"`
	InstructionAddendum string                          `json:"instructionAddendum,omitempty"`
}

// RecordContainedParentRecovery preserves original context and supervised usage
// after verified tree import. Repeated recovery must reproduce the same receipt.
func RecordContainedParentRecovery(writer *journal.Run, scope journal.Event, digest string, custody ContainedParentWorkspaceCustody, env apiv1.InvocationEnvelope, transcript *apiv1.ArtifactPointer, usage map[string]float64) error {
	record := containedParentRecovery{Version: 1, ContractDigest: digest, Custody: custody, Context: env.ContextPointers, Transcript: transcript, Usage: usage, InstructionAddendum: env.InstructionAddendum}
	data, err := json.Marshal(record)
	if err != nil || len(data) > maxParentRecoveryBytes || len(record.Context) > 128 || len(record.Usage) > 128 {
		return errors.New("parent recovery context exceeds bound")
	}
	reader, err := journal.OpenReadOnly(writer.Dir())
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["kind"] == ContainedParentRecoveredKind && event.Runner["contractDigest"] == digest {
			var ref journal.Ref
			raw, _ := json.Marshal(event.Runner["recovery"])
			if json.Unmarshal(raw, &ref) != nil || ref.Digest != journal.Digest(data) || event.Stage != scope.Stage || event.Attempt != scope.Attempt || event.Branch != scope.Branch {
				return errors.New("parent recovery receipt changed")
			}
			return nil
		}
	}
	ref, err := writer.RecordArtifactBoundedWithIntegrity(fmt.Sprintf("parent-recovery/%s-%d.json", scope.Stage, scope.Seq), data, apiv1.IntegrityTrusted, maxParentRecoveryBytes)
	if err != nil {
		return err
	}
	if ref.Digest != journal.Digest(data) {
		return errors.New("parent recovery context changed at journal boundary")
	}
	return writer.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: scope.Stage, Attempt: scope.Attempt, Branch: scope.Branch, Runner: map[string]any{"kind": ContainedParentRecoveredKind, "contractDigest": digest, "recovery": ref}})
}

func readParentRecovery(reader *journal.Reader, event journal.Event, runID string) (containedParentRecovery, error) {
	var record containedParentRecovery
	var ref journal.Ref
	raw, _ := json.Marshal(event.Runner["recovery"])
	if json.Unmarshal(raw, &ref) != nil || ref.Integrity != apiv1.IntegrityTrusted {
		return record, errors.New("invalid parent recovery ref")
	}
	data, err := reader.ArtifactBytesBounded(ref, maxParentRecoveryBytes)
	if err != nil {
		return record, err
	}
	if json.Unmarshal(data, &record) != nil || record.Version != 1 || record.ContractDigest != event.Runner["contractDigest"] || record.Custody.Version != 1 || record.Custody.Origin == nil || record.Custody.Workspace.OwnerRunID != runID || len(record.Context) > 128 || len(record.Usage) > 128 {
		return record, errors.New("invalid parent recovery custody")
	}
	return record, nil
}
