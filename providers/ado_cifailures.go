package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// adoPolicyEvaluationSettings is the subset of a policy configuration's
// settings that names what a CI policy requires: the build policy's display
// name and definition, or the status policy's genre and name.
type adoPolicyEvaluationSettings struct {
	DisplayName       string      `json:"displayName"`
	BuildDefinitionID json.Number `json:"buildDefinitionId"`
	StatusGenre       string      `json:"statusGenre"`
	StatusName        string      `json:"statusName"`
}

var _ PullRequestCIFailureReader = (*ADOProvider)(nil)

// PullRequestCIFailures returns the native CI evidence Azure DevOps offers for
// a pull request (design ado-parity-dsl-2-0.md §3.4, ADO-N22; #5652): every
// enabled, blocking build, status or unclassified policy evaluation that is
// rejected or broken. Reviewer, comment-resolution and work-item-linking
// policies are never CI failures and are left out, as are advisory
// (non-blocking) policies.
//
// For each failure it finds the build behind the policy (context.buildId, the
// build policy's latest build for refs/pull/<id>/merge, or the build a
// required pull request status links to), rejects a build of another
// repository or pull request, grades a build of another source head stale,
// and reports the build's failed jobs and tasks: their timeline issues and a
// bounded excerpt of each step's log tail, as annotations. Every failure is
// graded (CIFailureDetail.Evidence), so an external status, a missing build
// or a truncated log is explicit rather than an empty result. An
// authentication failure is returned as an error, never as empty evidence.
//
// Evaluations belong to the pull request, not to a commit, so the result is
// stamped with the source head ADO reports for the pull request; the caller
// compares it with the head it expected. The read is side-effect free.
func (p *ADOProvider) PullRequestCIFailures(ctx context.Context, repo RepositoryRef, pullID string) (PullRequestCIFailures, error) {
	if err := requireRepo(repo); err != nil {
		return PullRequestCIFailures{}, err
	}
	detail, err := p.getPullRequestDetail(ctx, repo, pullID)
	if err != nil {
		return PullRequestCIFailures{}, err
	}
	evals, err := p.pullRequestEvaluations(ctx, repo, pullID, detail)
	if err != nil {
		return PullRequestCIFailures{}, err
	}
	projectName := detail.Repository.Project.Name
	if projectName == "" {
		projectName = p.project(repo)
	}
	scope := &adoCIScope{repo: repo, pullID: pullID, detail: detail, project: projectName}
	failures, err := p.ciFailuresFromEvaluations(ctx, scope, evals)
	if err != nil {
		return PullRequestCIFailures{}, err
	}
	return PullRequestCIFailures{
		HeadSHA:  detail.LastMergeSourceCommit.CommitID,
		Failures: failures,
	}, nil
}

// ciFailuresFromEvaluations keeps the failing, blocking evaluations that
// report CI: build, status and unclassified policies, each with its collected
// evidence. Unlike reducePolicyEvaluations it takes no human-only
// configuration ids (a CI-poll gate input gather-ci-failures does not
// receive), so a rejected blocking policy a loop declares human-only (merge
// strategy, proof-of-presence) is still reported here, labelled by its
// policy type.
func (p *ADOProvider) ciFailuresFromEvaluations(ctx context.Context, scope *adoCIScope, evals []adoPolicyEvaluation) ([]CIFailureDetail, error) {
	failing := failingCIEvaluations(evals)
	failures := make([]CIFailureDetail, 0, len(failing))
	for _, ev := range failing {
		evidence, err := p.ciEvidenceFor(ctx, scope, ev)
		if err != nil {
			return nil, fmt.Errorf("collect CI evidence for %s: %w", adoCIPolicyName(ev), err)
		}
		link := p.buildResultsURL(scope.project, ev.Context.BuildID.String())
		if link == "" {
			link = evidence.buildURL
		}
		failures = append(failures, CIFailureDetail{
			CheckDetail: CheckDetail{
				Name:       adoCIPolicyName(ev),
				State:      CheckStateFailing,
				Conclusion: ev.Status,
				URL:        link,
				Summary:    evidence.summary(adoCIPolicySummary(ev)),
			},
			Annotations: evidence.annotations,
			Integrity:   apiintegrity.Unapproved,
			Evidence:    evidence.state,
		})
	}
	return failures, nil
}

// failingCIEvaluations keeps the enabled, blocking, rejected or broken
// evaluations of CI-gating policy kinds.
func failingCIEvaluations(evals []adoPolicyEvaluation) []adoPolicyEvaluation {
	var out []adoPolicyEvaluation
	for _, ev := range evals {
		if !adoEvaluationBlocks(ev) || adoPolicyCheckState(ev.Status) != CheckStateFailing {
			continue
		}
		if adoPolicyKindOf(ev.Configuration.Type.ID).gatesCI() {
			out = append(out, ev)
		}
	}
	return out
}

// HasPullRequestCIFailures reports whether PullRequestCIFailures would report
// at least one failure for the pull request, from the same policy
// evaluations but without reading any build, timeline or log. It costs the
// same two GETs as before #5652.
func (p *ADOProvider) HasPullRequestCIFailures(ctx context.Context, repo RepositoryRef, pullID string) (bool, error) {
	if err := requireRepo(repo); err != nil {
		return false, err
	}
	detail, err := p.getPullRequestDetail(ctx, repo, pullID)
	if err != nil {
		return false, err
	}
	evals, err := p.pullRequestEvaluations(ctx, repo, pullID, detail)
	if err != nil {
		return false, err
	}
	return len(failingCIEvaluations(evals)) > 0, nil
}

// adoCIPolicyName labels a CI policy by what it requires when ADO says so
// (the build policy's display name, or the genre/name of a status policy),
// and otherwise by its policy type and configuration id, so several unnamed
// policies of one type stay distinguishable.
func adoCIPolicyName(ev adoPolicyEvaluation) string {
	settings := ev.Configuration.Settings
	typeName := adoPolicyName(ev)
	if name := strings.TrimSpace(settings.DisplayName); name != "" {
		return typeName + ": " + name
	}
	status := strings.TrimSpace(settings.StatusName)
	if status == "" {
		if id := ev.Configuration.ID.String(); id != "" {
			return typeName + " #" + id
		}
		return typeName
	}
	if genre := strings.TrimSpace(settings.StatusGenre); genre != "" {
		status = genre + "/" + status
	}
	return typeName + ": " + status
}

// adoCIPolicySummary says what ADO reported about the policy itself; the
// collected evidence is appended after it.
func adoCIPolicySummary(ev adoPolicyEvaluation) string {
	if strings.EqualFold(ev.Status, "broken") {
		return "Azure DevOps could not evaluate this blocking policy (broken)"
	}
	return "blocking policy rejected by Azure DevOps"
}
