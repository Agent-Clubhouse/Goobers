package childworkflow

import (
	"encoding/json"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// ChildStartKind distinguishes generated definitions from catalog triggers.
// Ordinary dispatch must refuse this envelope until a child launcher is wired.
const ChildStartKind = "child-workflow"

// ChildStartEnvelope pins execution inputs without embedding authored source,
// policy, credentials or a mutable catalog alias. Grant/attempt nonces are absent
// so an authorized replacement attempt can recover the same invocation receipt.
type ChildStartEnvelope struct {
	Kind                 string  `json:"kind"`
	Version              int     `json:"version"`
	Gaggle               string  `json:"gaggle"`
	ParentRunID          string  `json:"parentRunId"`
	ParentStage          string  `json:"parentStage"`
	StageOccurrence      string  `json:"stageOccurrence"`
	InvocationKey        string  `json:"invocationKey"`
	ConfigGeneration     string  `json:"configGeneration"`
	ParentWorkflow       string  `json:"parentWorkflow"`
	ParentWorkflowDigest string  `json:"parentWorkflowDigest"`
	ParentGooberDigest   string  `json:"parentGooberDigest"`
	Workflow             string  `json:"workflow"`
	SourceDigest         string  `json:"sourceDigest"`
	CanonicalDigest      string  `json:"canonicalDigest"`
	ConfigDigest         string  `json:"configDigest"`
	PolicyDigest         string  `json:"policyDigest"`
	WorkflowDigest       string  `json:"workflowDigest"`
	PlacementsDigest     string  `json:"placementsDigest"`
	Backend              Backend `json:"backend"`
	MaxChildren          int     `json:"maxChildren"`
}

// Identity is the only durable namespace, shared with ordinary queue custody.
func (e ChildStartEnvelope) Identity() triggerqueue.ChildIdentity {
	return triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: e.Gaggle, ParentRunID: e.ParentRunID}, StageOccurrence: e.StageOccurrence, InvocationKey: e.InvocationKey}
}

func childStartEnvelope(a Authority, p *Proposal, invocationKey string) (ChildStartEnvelope, error) {
	plans, err := json.Marshal(p.Placements)
	if err != nil {
		return ChildStartEnvelope{}, err
	}
	return ChildStartEnvelope{
		Kind: ChildStartKind, Version: 1, Gaggle: a.Origin.Gaggle, ParentRunID: a.Origin.RunID,
		ParentStage: a.Admission.ParentTask.Name, StageOccurrence: a.Origin.StageOccurrence, InvocationKey: invocationKey,
		ConfigGeneration: a.ConfigGeneration, ParentWorkflow: a.ParentWorkflow, ParentWorkflowDigest: a.ParentWorkflowDigest, ParentGooberDigest: a.ParentGooberDigest,
		Workflow: p.Workflow.Name, SourceDigest: p.SourceDigest, CanonicalDigest: p.CanonicalDigest,
		ConfigDigest: p.ConfigDigest, PolicyDigest: p.PolicyDigest, WorkflowDigest: p.Machine.Digest(), PlacementsDigest: digest(plans),
		Backend: a.Admission.Backend, MaxChildren: int(a.Admission.ParentTask.ChildWorkflows.EffectiveMaxChildren()),
	}, nil
}
