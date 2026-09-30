package providers

import (
	"context"
	"strings"

	apiintegrity "github.com/goobers/goobers/api/integrity"
)

// adoPolicyEvaluationSettings is the subset of a policy configuration's
// settings that names what a CI policy requires: the build policy's display
// name, or the status policy's genre and name.
type adoPolicyEvaluationSettings struct {
	DisplayName string `json:"displayName"`
	StatusGenre string `json:"statusGenre"`
	StatusName  string `json:"statusName"`
}

var _ PullRequestCIFailureReader = (*ADOProvider)(nil)

// PullRequestCIFailures returns the minimal native CI evidence Azure DevOps
// offers for a pull request (design ado-parity-dsl-2-0.md §3.4, ADO-N22):
// every enabled, blocking build, status or unclassified policy evaluation that
// is rejected or broken, with a link to the evaluated build from
// context.buildId. Reviewer, comment-resolution and work-item-linking
// policies are never CI failures and are left out, as are advisory
// (non-blocking) policies. Build logs are not fetched.
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
	return PullRequestCIFailures{
		HeadSHA:  detail.LastMergeSourceCommit.CommitID,
		Failures: p.ciFailuresFromEvaluations(evals, projectName),
	}, nil
}

// ciFailuresFromEvaluations keeps the failing, blocking evaluations that
// report CI: build, status and unclassified policies. Unlike
// reducePolicyEvaluations it takes no human-only configuration ids (a CI-poll
// gate input gather-ci-failures does not receive), so a rejected blocking
// policy a loop declares human-only (merge strategy, proof-of-presence) is
// still reported here, labelled by its policy type.
func (p *ADOProvider) ciFailuresFromEvaluations(evals []adoPolicyEvaluation, projectName string) []CIFailureDetail {
	failures := make([]CIFailureDetail, 0, len(evals))
	for _, ev := range evals {
		if !adoEvaluationBlocks(ev) || adoPolicyCheckState(ev.Status) != CheckStateFailing {
			continue
		}
		if !adoPolicyKindOf(ev.Configuration.Type.ID).gatesCI() {
			continue
		}
		failures = append(failures, CIFailureDetail{
			CheckDetail: CheckDetail{
				Name:       adoCIPolicyName(ev),
				State:      CheckStateFailing,
				Conclusion: ev.Status,
				URL:        p.buildResultsURL(projectName, ev.Context.BuildID.String()),
				Summary:    adoCIPolicySummary(ev),
			},
			Annotations: []CheckAnnotation{},
			Integrity:   apiintegrity.Unapproved,
		})
	}
	return failures
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

// adoCIPolicySummary says what ADO reported. Minimal evidence: no log fetch.
// Only a build policy mentions build logs; other policies have none.
func adoCIPolicySummary(ev adoPolicyEvaluation) string {
	summary := "blocking policy rejected by Azure DevOps"
	if strings.EqualFold(ev.Status, "broken") {
		summary = "Azure DevOps could not evaluate this blocking policy (broken)"
	}
	if strings.EqualFold(strings.TrimSpace(ev.Configuration.Type.ID), adoPolicyTypeBuild) {
		summary += "; build logs are not fetched"
	}
	return summary
}
