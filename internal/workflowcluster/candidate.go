package workflowcluster

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Proposal kind and target stamped on every advisory TemplateChangeRequest.
const (
	// ProposalKindConfigChange marks a proposal as a reviewed config change request.
	ProposalKindConfigChange = "config-change-request"
	// ProposalTargetCatalog names the child-authoring catalog the change targets.
	ProposalTargetCatalog = "child-authoring-catalog"
)

// Suppression reasons reported in CandidateSuppression.Reason.
const (
	// SuppressedUnstable means the cluster is unstable or below the stability bar.
	SuppressedUnstable = "low-stability"
	// SuppressedLowSupport means too few parent runs, repositories, or joined evidence.
	SuppressedLowSupport = "low-support"
	// SuppressedWorseOutcomes means a task cohort underperforms comparable alternatives.
	SuppressedWorseOutcomes = "worse-outcomes"
	// SuppressedFindings means a member carries policy or safety findings.
	SuppressedFindings = "policy-or-safety-findings"
	// SuppressedNoComparator means a task cohort has no alternative-approach outcomes.
	SuppressedNoComparator = "no-comparable-alternative"
	// SuppressedNoParameters means the medoid exposes no reviewed template parameters.
	SuppressedNoParameters = "medoid-not-parameterized"
)

// CandidateOptions defines the evidence bar for proposing a reusable template.
// Zero values select conservative defaults.
type CandidateOptions struct {
	MinStability    float64 `json:"minStability"`
	MinParentRuns   int     `json:"minParentRuns"`
	MinRepositories int     `json:"minRepositories"`
}

func (o CandidateOptions) withDefaults(m Model) CandidateOptions {
	if o.MinStability <= 0 {
		o.MinStability = m.Options.StabilityThreshold
		if o.MinStability <= 0 {
			o.MinStability = 0.6
		}
	}
	if o.MinParentRuns <= 0 {
		o.MinParentRuns = 3
	}
	if o.MinRepositories <= 0 {
		o.MinRepositories = 2
	}
	return o
}

// TemplateParameter is a reviewable parameter exposed by the medoid template.
type TemplateParameter struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// CandidateEvidence joins one retained child proposal to its run outcome and
// parent context. ProposalID must match the corresponding clustering point ID.
type CandidateEvidence struct {
	ProposalID     string              `json:"proposalId"`
	ProposalDigest string              `json:"proposalDigest"`
	RunID          string              `json:"runId"`
	ParentRunID    string              `json:"parentRunId"`
	Repository     string              `json:"repository"`
	TaskCohort     string              `json:"taskCohort"`
	Succeeded      bool                `json:"succeeded"`
	PolicyFindings []string            `json:"policyFindings,omitempty"`
	SafetyFindings []string            `json:"safetyFindings,omitempty"`
	Parameters     []TemplateParameter `json:"parameters,omitempty"`
}

// OutcomeComparison records the like-for-like task-cohort comparison that
// qualified a candidate.
type OutcomeComparison struct {
	CandidateRuns   int      `json:"candidateRuns"`
	CandidateRate   float64  `json:"candidateRate"`
	AlternativeRuns int      `json:"alternativeRuns"`
	AlternativeRate float64  `json:"alternativeRate"`
	TaskCohorts     []string `json:"taskCohorts"`
}

// TemplateChangeRequest is advisory output for the existing review and
// promotion path. It never writes to the configured catalog.
type TemplateChangeRequest struct {
	Kind                  string              `json:"kind"`
	Target                string              `json:"target"`
	TemplateName          string              `json:"templateName"`
	SourceProposalID      string              `json:"sourceProposalId"`
	SourceProposalDigest  string              `json:"sourceProposalDigest"`
	Parameters            []TemplateParameter `json:"parameters,omitempty"`
	ReviewRequired        bool                `json:"reviewRequired"`
	AutomaticRegistration bool                `json:"automaticRegistration"`
}

// TemplateCandidate is a fully traceable proposal backed by one stable cluster.
type TemplateCandidate struct {
	ModelID        string                `json:"modelId"`
	ClusterID      string                `json:"clusterId"`
	Stability      float64               `json:"stability"`
	Medoid         Point                 `json:"medoid"`
	Distinguishing []Attribution         `json:"distinguishing,omitempty"`
	Evidence       []CandidateEvidence   `json:"evidence"`
	Outcomes       OutcomeComparison     `json:"outcomes"`
	Proposal       TemplateChangeRequest `json:"proposal"`
}

// CandidateSuppression explains why a cluster did not clear the evidence bar.
type CandidateSuppression struct {
	ClusterID string `json:"clusterId"`
	Reason    string `json:"reason"`
	Detail    string `json:"detail"`
}

// CandidateReport contains advisory proposals and every suppression decision.
type CandidateReport struct {
	Options    CandidateOptions       `json:"options"`
	Candidates []TemplateCandidate    `json:"candidates"`
	Suppressed []CandidateSuppression `json:"suppressed"`
}

// ProposeTemplates evaluates structural clusters without changing config.
func ProposeTemplates(model Model, evidence []CandidateEvidence, opts CandidateOptions) (CandidateReport, error) {
	if model.FeatureVersion != StructuralFeatureVersion {
		return CandidateReport{}, fmt.Errorf("workflowcluster: template candidates require %s, got %q", StructuralFeatureVersion, model.FeatureVersion)
	}
	opts = opts.withDefaults(model)
	report := CandidateReport{Options: opts, Candidates: []TemplateCandidate{}, Suppressed: []CandidateSuppression{}}

	byProposal := make(map[string]CandidateEvidence, len(evidence))
	for _, item := range evidence {
		if item.ProposalID == "" || item.ProposalDigest == "" || item.RunID == "" || item.ParentRunID == "" ||
			item.Repository == "" || item.TaskCohort == "" {
			return CandidateReport{}, errors.New("workflowcluster: candidate evidence requires proposal, digest, run, parent, repository, and task cohort")
		}
		if _, exists := byProposal[item.ProposalID]; exists {
			return CandidateReport{}, fmt.Errorf("workflowcluster: duplicate candidate evidence for %q", item.ProposalID)
		}
		byProposal[item.ProposalID] = item
	}

	for _, cluster := range model.Clusters {
		candidate, suppression := evaluateCluster(model, cluster, byProposal, opts)
		if suppression != nil {
			report.Suppressed = append(report.Suppressed, *suppression)
			continue
		}
		report.Candidates = append(report.Candidates, candidate)
	}
	return report, nil
}

func evaluateCluster(model Model, cluster Cluster, evidence map[string]CandidateEvidence, opts CandidateOptions) (TemplateCandidate, *CandidateSuppression) {
	suppress := func(reason, detail string) (TemplateCandidate, *CandidateSuppression) {
		return TemplateCandidate{}, &CandidateSuppression{ClusterID: cluster.ID, Reason: reason, Detail: detail}
	}
	if !cluster.Stable || cluster.Stability < opts.MinStability {
		return suppress(SuppressedUnstable, fmt.Sprintf("stability %.3f is below %.3f", cluster.Stability, opts.MinStability))
	}

	members := make([]CandidateEvidence, 0, len(cluster.Members))
	parents, repositories, cohorts := map[string]bool{}, map[string]bool{}, map[string]bool{}
	successes := 0
	candidateByCohort := map[string][2]int{}
	for _, id := range cluster.Members {
		item, ok := evidence[id]
		if !ok {
			return suppress(SuppressedLowSupport, fmt.Sprintf("member %q has no joined proposal/run evidence", id))
		}
		members = append(members, item)
		parents[item.ParentRunID], repositories[item.Repository], cohorts[item.TaskCohort] = true, true, true
		if item.Succeeded {
			successes++
		}
		counts := candidateByCohort[item.TaskCohort]
		counts[0]++
		if item.Succeeded {
			counts[1]++
		}
		candidateByCohort[item.TaskCohort] = counts
		if len(item.PolicyFindings) > 0 || len(item.SafetyFindings) > 0 {
			return suppress(SuppressedFindings, fmt.Sprintf("member %q has policy or safety findings", id))
		}
	}
	if len(parents) < opts.MinParentRuns || len(repositories) < opts.MinRepositories {
		return suppress(SuppressedLowSupport, fmt.Sprintf(
			"support spans %d parent runs and %d repositories; need %d and %d",
			len(parents), len(repositories), opts.MinParentRuns, opts.MinRepositories,
		))
	}

	memberIDs := make(map[string]bool, len(members))
	for _, item := range members {
		memberIDs[item.ProposalID] = true
	}
	alternativeRuns, alternativeSuccesses := 0, 0
	alternativeByCohort := map[string][2]int{}
	for id, item := range evidence {
		if memberIDs[id] || !cohorts[item.TaskCohort] {
			continue
		}
		alternativeRuns++
		if item.Succeeded {
			alternativeSuccesses++
		}
		counts := alternativeByCohort[item.TaskCohort]
		counts[0]++
		if item.Succeeded {
			counts[1]++
		}
		alternativeByCohort[item.TaskCohort] = counts
	}
	for _, cohort := range sortedKeys(candidateByCohort) {
		candidateCounts := candidateByCohort[cohort]
		alternativeCounts := alternativeByCohort[cohort]
		if alternativeCounts[0] == 0 {
			return suppress(SuppressedNoComparator, fmt.Sprintf("task cohort %q has no outcomes from another approach", cohort))
		}
		candidateRate := float64(candidateCounts[1]) / float64(candidateCounts[0])
		alternativeRate := float64(alternativeCounts[1]) / float64(alternativeCounts[0])
		if candidateRate < alternativeRate {
			return suppress(SuppressedWorseOutcomes, fmt.Sprintf(
				"task cohort %q success rate %.3f is below comparable alternatives %.3f",
				cohort, candidateRate, alternativeRate,
			))
		}
	}
	candidateRate := float64(successes) / float64(len(members))
	alternativeRate := float64(alternativeSuccesses) / float64(alternativeRuns)

	medoid, ok := evidence[cluster.Medoid.ID]
	if !ok {
		return suppress(SuppressedLowSupport, fmt.Sprintf("medoid %q has no joined proposal/run evidence", cluster.Medoid.ID))
	}
	if len(medoid.Parameters) == 0 {
		return suppress(SuppressedNoParameters, fmt.Sprintf("medoid %q has no reviewed template parameters", cluster.Medoid.ID))
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ProposalID < members[j].ProposalID })
	taskCohorts := sortedKeys(cohorts)
	return TemplateCandidate{
		ModelID: model.ID, ClusterID: cluster.ID, Stability: cluster.Stability, Medoid: cluster.Medoid,
		Distinguishing: append([]Attribution(nil), cluster.Distinguishing...), Evidence: members,
		Outcomes: OutcomeComparison{
			CandidateRuns: len(members), CandidateRate: candidateRate,
			AlternativeRuns: alternativeRuns, AlternativeRate: alternativeRate, TaskCohorts: taskCohorts,
		},
		Proposal: TemplateChangeRequest{
			Kind: ProposalKindConfigChange, Target: ProposalTargetCatalog,
			TemplateName:     candidateTemplateName(model.ID, cluster.ID),
			SourceProposalID: medoid.ProposalID, SourceProposalDigest: medoid.ProposalDigest,
			Parameters:     append([]TemplateParameter(nil), medoid.Parameters...),
			ReviewRequired: true, AutomaticRegistration: false,
		},
	}, nil
}

func candidateTemplateName(modelID, clusterID string) string {
	modelID = strings.TrimPrefix(modelID, "sha256:")
	if len(modelID) > 12 {
		modelID = modelID[:12]
	}
	return "child-" + modelID + "-" + clusterID
}
