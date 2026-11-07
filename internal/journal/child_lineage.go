package journal

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ChildLineage identifies the exact durable child invocation and its retained
// proposal. SourceDigest is resolved only with this complete gaggle/parent/
// occurrence/key tuple in triggerqueue; a digest alone does not grant access.
// EnvelopeDigest commits to every pinned start input, including policy and
// placement, without copying credentials or authored source into run.yaml.
type ChildLineage struct {
	Gaggle          string        `json:"gaggle"`
	ParentRunID     string        `json:"parentRunId"`
	ParentWorkflow  string        `json:"parentWorkflow"`
	StageOccurrence string        `json:"stageOccurrence"`
	InvocationKey   string        `json:"invocationKey"`
	AcceptanceID    string        `json:"acceptanceId"`
	SourceDigest    string        `json:"sourceDigest"`
	EnvelopeDigest  string        `json:"envelopeDigest"`
	Novelty         *ChildNovelty `json:"novelty,omitempty"`
}

// ChildNovelty is the bounded, advisory cluster assignment pinned at child
// admission. It is evidence only and grants no workflow authority.
type ChildNovelty struct {
	Scored                bool                    `json:"scored"`
	Reason                string                  `json:"reason,omitempty"`
	ModelID               string                  `json:"modelId,omitempty"`
	ModelVersion          string                  `json:"modelVersion,omitempty"`
	FeatureVersion        string                  `json:"featureVersion,omitempty"`
	ClusterID             string                  `json:"clusterId,omitempty"`
	Nearest               string                  `json:"nearest,omitempty"`
	NearestMedoid         string                  `json:"nearestMedoid,omitempty"`
	NearestSize           int                     `json:"nearestSize,omitempty"`
	NearestStable         bool                    `json:"nearestStable,omitempty"`
	Distance              float64                 `json:"distance"`
	OutOfDistribution     bool                    `json:"outOfDistribution"`
	HistoricalOutcome     *ChildHistoricalOutcome `json:"historicalOutcome,omitempty"`
	HumanApprovalRequired bool                    `json:"humanApprovalRequired,omitempty"`
	// HumanApproval records the reviewed decision that released a held
	// out-of-distribution child. It is present only when approval was required.
	HumanApproval *ChildHumanApproval `json:"humanApproval,omitempty"`
}

// ChildHumanApproval is the durable operator decision that admitted a held child.
type ChildHumanApproval struct {
	Actor      string    `json:"actor"`
	ApprovedAt time.Time `json:"approvedAt"`
}

// ChildHistoricalOutcome is the admission-time outcome snapshot for a cluster.
type ChildHistoricalOutcome struct {
	RunCount         int     `json:"runCount"`
	SuccessRate      float64 `json:"successRate"`
	InsufficientRuns bool    `json:"insufficientRuns"`
}

// Equal compares lineage by value, including pinned advisory evidence.
func (c ChildLineage) Equal(other ChildLineage) bool {
	return reflect.DeepEqual(c, other)
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
		apiv1.ValidRunID(id.RunID) && c.AcceptanceID == "trigger-"+id.RunID &&
		childIdentityText(c.StageOccurrence, 256) && childIdentityText(c.InvocationKey, 256) &&
		id.ContinuedFromRunID == ""
	if !valid {
		return errors.New("journal: invalid child lineage")
	}
	for _, value := range []string{c.SourceDigest, c.EnvelopeDigest, id.ConfigGeneration, id.WorkflowDigest, id.GooberDigest} {
		if _, err := digestHex(value); err != nil || strings.ToLower(value) != value {
			return errors.New("journal: invalid child execution digest")
		}
	}
	if c.Novelty != nil && !c.Novelty.Valid() {
		return errors.New("journal: invalid child novelty evidence")
	}
	return nil
}

// Valid reports whether advisory evidence is bounded and internally consistent.
func (n ChildNovelty) Valid() bool {
	for _, value := range []string{n.Reason, n.ModelID, n.ModelVersion, n.FeatureVersion, n.ClusterID, n.Nearest, n.NearestMedoid} {
		if len(value) > 256 || !utf8.ValidString(value) || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return false
		}
	}
	if math.IsNaN(n.Distance) || math.IsInf(n.Distance, 0) || n.Distance < 0 || n.Distance > 1 || n.NearestSize < 0 {
		return false
	}
	if n.HistoricalOutcome != nil {
		outcome := n.HistoricalOutcome
		if outcome.RunCount < 0 || math.IsNaN(outcome.SuccessRate) || math.IsInf(outcome.SuccessRate, 0) ||
			outcome.SuccessRate < 0 || outcome.SuccessRate > 1 {
			return false
		}
	}
	if a := n.HumanApproval; a != nil && (!n.HumanApprovalRequired || !childIdentityText(a.Actor, 1024) || a.ApprovedAt.IsZero()) {
		return false
	}
	return !n.HumanApprovalRequired || n.Scored && n.OutOfDistribution
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
