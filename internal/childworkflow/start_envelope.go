package childworkflow

import (
	"encoding/json"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workflowcluster"
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
	// ExecutionDigest pins the parent's ExecutionBinding. Engine envelopes
	// require it; runner envelopes omit it and keep their original encoding.
	ExecutionDigest string `json:"executionDigest,omitempty"`
	// Novelty is pinned advisory evidence and grants no execution authority.
	Novelty *workflowcluster.Novelty `json:"novelty,omitempty"`
}

// ExecutionBinding is where an engine child acts: the parent run's instance,
// repository and run-branch namespace. It is security-relevant and is never
// taken from a resolver without matching the digest pinned at acceptance.
type ExecutionBinding struct {
	InstanceID      string        `json:"instanceId"`
	RepoRef         apiv1.RepoRef `json:"repoRef"`
	BranchNamespace string        `json:"branchNamespace"`
}

// Digest is the binding's acceptance-time identity.
func (b ExecutionBinding) Digest() (string, error) {
	data, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	return digest(data), nil
}

// Identity is the only durable namespace, shared with ordinary queue custody.
func (e ChildStartEnvelope) Identity() triggerqueue.ChildIdentity {
	return triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: e.Gaggle, ParentRunID: e.ParentRunID}, StageOccurrence: e.StageOccurrence, InvocationKey: e.InvocationKey}
}

// JournalNovelty converts admission evidence into the dependency-free journal
// projection used by both local and engine child starts.
func (e ChildStartEnvelope) JournalNovelty() *journal.ChildNovelty {
	if e.Novelty == nil {
		return nil
	}
	n := e.Novelty
	return &journal.ChildNovelty{
		Scored: n.Scored, Reason: n.Reason, ModelID: n.ModelID, ModelVersion: n.ModelVersion,
		FeatureVersion: n.FeatureVersion, ClusterID: n.ClusterID, Nearest: n.Nearest,
		NearestMedoid: n.NearestMedoid, NearestSize: n.NearestSize, NearestStable: n.NearestStable,
		Distance: n.Distance, OutOfDistribution: n.OutOfDistribution,
		HumanApprovalRequired: n.HumanApprovalRequired,
		HistoricalOutcome:     journalChildOutcome(n.HistoricalOutcome),
	}
}

func journalChildOutcome(outcome *workflowcluster.HistoricalOutcome) *journal.ChildHistoricalOutcome {
	if outcome == nil {
		return nil
	}
	return &journal.ChildHistoricalOutcome{
		RunCount: outcome.RunCount, SuccessRate: outcome.SuccessRate, InsufficientRuns: outcome.InsufficientRuns,
	}
}

func childStartEnvelope(a Authority, p *Proposal, invocationKey string) (ChildStartEnvelope, error) {
	plans, err := json.Marshal(p.Placements)
	if err != nil {
		return ChildStartEnvelope{}, err
	}
	executionDigest := ""
	if a.Admission.Backend == BackendEngine {
		if a.Execution == nil {
			return ChildStartEnvelope{}, errors.New("childworkflow: an engine child requires its parent's execution binding")
		}
		if executionDigest, err = a.Execution.Digest(); err != nil {
			return ChildStartEnvelope{}, err
		}
	}
	return ChildStartEnvelope{
		ExecutionDigest: executionDigest,
		Kind:            ChildStartKind, Version: 1, Gaggle: a.Origin.Gaggle, ParentRunID: a.Origin.RunID,
		ParentStage: a.Admission.ParentTask.Name, StageOccurrence: a.Origin.StageOccurrence, InvocationKey: invocationKey,
		ConfigGeneration: a.ConfigGeneration, ParentWorkflow: a.ParentWorkflow, ParentWorkflowDigest: a.ParentWorkflowDigest, ParentGooberDigest: a.ParentGooberDigest,
		Workflow: p.Workflow.Name, SourceDigest: p.SourceDigest, CanonicalDigest: p.CanonicalDigest,
		ConfigDigest: p.ConfigDigest, PolicyDigest: p.PolicyDigest, WorkflowDigest: p.Machine.Digest(), PlacementsDigest: digest(plans),
		Backend: a.Admission.Backend, MaxChildren: int(a.Admission.ParentTask.ChildWorkflows.EffectiveMaxChildren()),
		Novelty: p.Novelty,
	}, nil
}
