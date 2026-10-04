package journal

import (
	"errors"
	"strings"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ChildLineage identifies the exact durable child invocation and its retained
// proposal. SourceDigest is resolved only with this complete gaggle/parent/
// occurrence/key tuple in triggerqueue; a digest alone does not grant access.
// EnvelopeDigest commits to every pinned start input, including policy and
// placement, without copying credentials or authored source into run.yaml.
type ChildLineage struct {
	Gaggle          string `json:"gaggle"`
	ParentRunID     string `json:"parentRunId"`
	ParentWorkflow  string `json:"parentWorkflow"`
	StageOccurrence string `json:"stageOccurrence"`
	InvocationKey   string `json:"invocationKey"`
	AcceptanceID    string `json:"acceptanceId"`
	SourceDigest    string `json:"sourceDigest"`
	EnvelopeDigest  string `json:"envelopeDigest"`
	ExecutionEpoch  int    `json:"executionEpoch,omitempty"`
	PriorResultRef  string `json:"priorResultRef,omitempty"`
	RestartDigest   string `json:"restartDigest,omitempty"`
}

// ValidateChildLineage refuses malformed or cross-gaggle generated provenance.
// Legacy identities without child provenance retain their previous behavior.
// Validation does not authorize execution; callers must verify queue custody,
// pinned definitions, live permissions and workspace/scheduler admission.
func (id RunIdentity) ValidateChildLineage() error {
	c := id.Child
	if c == nil {
		return nil
	}
	valid := c.Gaggle == id.Gaggle && childIdentityText(c.Gaggle, 128) &&
		childIdentityText(c.ParentWorkflow, 256) && apiv1.ValidRunID(c.ParentRunID) && c.ParentRunID != id.RunID &&
		apiv1.ValidRunID(id.RunID) &&
		childIdentityText(c.StageOccurrence, 256) && childIdentityText(c.InvocationKey, 256) && validChildExecutionLineage(id)
	if !valid {
		return errors.New("journal: invalid child lineage")
	}
	for _, value := range []string{c.SourceDigest, c.EnvelopeDigest, id.ConfigGeneration, id.WorkflowDigest, id.GooberDigest} {
		if _, err := digestHex(value); err != nil || strings.ToLower(value) != value {
			return errors.New("journal: invalid child execution digest")
		}
	}
	return nil
}

func childIdentityText(value string, max int) bool {
	if value == "" || len(value) > max || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// validChildExecutionLineage distinguishes original acceptance from a linked
// human epoch. Queue admission must independently bind RestartDigest to custody.
func validChildExecutionLineage(id RunIdentity) bool {
	c := id.Child
	if c.ExecutionEpoch == 0 {
		return c.AcceptanceID == "trigger-"+id.RunID && id.ContinuedFromRunID == "" && c.PriorResultRef == "" && c.RestartDigest == ""
	}
	accepted := strings.TrimPrefix(c.AcceptanceID, "trigger-")
	if c.ExecutionEpoch < 1 || c.ExecutionEpoch > 8 || accepted == c.AcceptanceID || !apiv1.ValidRunID(accepted) || accepted == id.RunID || !apiv1.ValidRunID(id.ContinuedFromRunID) || id.ContinuedFromRunID == id.RunID || id.ContinuedFromRunID == c.ParentRunID || (c.ExecutionEpoch == 1) != (id.ContinuedFromRunID == accepted) || id.SourceTerminalSeq == 0 || id.Operator == "" || id.RequestedTarget == "" {
		return false
	}
	if _, err := digestHex(c.RestartDigest); err != nil {
		return false
	}
	if _, err := digestHex(c.PriorResultRef); err != nil {
		return false
	}
	return strings.ToLower(c.RestartDigest) == c.RestartDigest && strings.ToLower(c.PriorResultRef) == c.PriorResultRef
}

func childContinuationLineage(source RunIdentity, requested *ChildLineage) (*ChildLineage, error) {
	if source.Child == nil {
		if requested != nil {
			return nil, errors.New("journal: ordinary continuation cannot acquire child provenance")
		}
		return nil, nil
	}
	if requested == nil {
		return nil, errors.New("journal: child continuation requires admitted execution lineage")
	}
	expected, actual := *source.Child, *requested
	expected.ExecutionEpoch++
	expected.PriorResultRef = actual.PriorResultRef
	expected.RestartDigest = actual.RestartDigest
	if expected != actual {
		return nil, errors.New("journal: child continuation changed accepted lineage")
	}
	return &actual, nil
}
