package providers

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ApplyPullRequestRepair uses ADO's exact oldObjectId lease. It does not rebase,
// create another branch, bypass policy, or clear any control marker.
// https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pushes/create?view=azure-devops-rest-7.1
func (p *ADOProvider) ApplyPullRequestRepair(ctx context.Context, value PullRequestRepair) (PRRepairResult, error) {
	var result PRRepairResult
	if err := ValidatePullRequestRepair(value); err != nil {
		return result, err
	}
	ctx, cancel := repairContext(ctx)
	defer cancel()
	if err := freshRepairTarget(ctx, p, value.Target); err != nil {
		return result, err
	}
	if err := validateRepairBefore(ctx, value, p.repairFile); err != nil {
		return result, err
	}
	changes := make([]adoChange, 0, len(value.Changes))
	for _, change := range value.Changes {
		kind := "edit"
		if change.PreviousBlob == "" {
			kind = "add"
		}
		var content *adoNewContent
		if change.Content == nil {
			kind = "delete"
		} else {
			content = &adoNewContent{Content: *change.Content, ContentType: "rawtext"}
		}
		changes = append(changes, adoChange{ChangeType: kind, Item: map[string]string{"path": "/" + change.Path}, NewContent: content})
	}
	name := "refs/heads/" + value.Target.Head
	body := adoPushRequest{RefUpdates: []adoRefUpdate{{Name: name, OldObjectID: value.Target.HeadSHA}}, Commits: []adoCommit{{Comment: value.Message, Changes: changes}}}
	endpoint, err := p.repoURL(value.Target.Repository, "pushes")
	if err != nil {
		return result, err
	}
	var out struct {
		Commits    []proposalADOCommit
		RefUpdates []proposalADORefResult
	}
	result.MutationAttempted = true
	if err = p.do(WithoutMutationRetries(ctx), http.MethodPost, endpoint, body, &out); err != nil {
		return result, err
	}
	if len(out.Commits) != 1 || len(out.RefUpdates) != 1 {
		return result, ErrPRRepair
	}
	commit, ref := out.Commits[0], out.RefUpdates[0]
	if !repairADOCommitMatches(commit, value) || ref.Name != name || ref.OldObjectID != value.Target.HeadSHA || ref.NewObjectID != commit.CommitID {
		return result, ErrPRRepair
	}
	result.Acknowledged, result.CommitID = true, commit.CommitID
	p.recordMutation(ctx, "pull-request", value.Target.ID, "interactive-repair", value.Target.Repository)
	return result, nil
}
func repairADOCommitMatches(commit proposalADOCommit, value PullRequestRepair) bool {
	return ValidSourceCommit(commit.CommitID) && ValidSourceCommit(commit.TreeID) && len(commit.Parents) == 1 && commit.Parents[0] == value.Target.HeadSHA && strings.TrimSuffix(commit.Comment, "\n") == strings.TrimSuffix(value.Message, "\n")
}

// ObservePullRequestRepair verifies exact command, ancestry and file delta.
func (p *ADOProvider) ObservePullRequestRepair(ctx context.Context, value PullRequestRepair) (PRRepairObservation, error) {
	var result PRRepairObservation
	if err := ValidatePullRequestRepair(value); err != nil {
		return result, err
	}
	ctx, cancel := repairContext(ctx)
	defer cancel()
	current, err := p.InspectRepairPullRequest(ctx, value.Target.Repository, value.Target.ID)
	if err != nil {
		return result, err
	}
	if !sameRepairTarget(value.Target, current) {
		return result, ErrPRRepair
	}
	if current.HeadSHA == value.Target.HeadSHA {
		return result, nil
	}
	commit, err := p.repairCommit(ctx, value.Target.Repository, current.HeadSHA)
	if err != nil {
		return result, err
	}
	if !repairADOCommitMatches(commit, value) {
		return result, ErrPRRepair
	}
	if err = p.verifyRepairChanges(ctx, value, current.HeadSHA); err != nil {
		return result, err
	}
	if err = verifyRepairFiles(ctx, value, current.HeadSHA, p.repairFile); err != nil {
		return result, err
	}
	return PRRepairObservation{Matches: true, CommitID: current.HeadSHA}, nil
}
func (p *ADOProvider) verifyRepairChanges(ctx context.Context, value PullRequestRepair, sha string) error {
	endpoint, err := p.repoURL(value.Target.Repository, "commits", sha, "changes")
	if err != nil {
		return err
	}
	endpoint, err = addQuery(endpoint, url.Values{"top": {strconv.Itoa(MaxPRRepairFiles + 1)}, "skip": {"0"}})
	if err != nil {
		return err
	}
	response, err := p.send(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return err
	}
	more := response.Header.Get("x-ms-continuationtoken") != "" || response.Header.Get("Link") != ""
	var out struct {
		ChangeCounts map[string]int
		Changes      []struct {
			ChangeType, OriginalPath string
			Item                     struct {
				Path, GitObjectType string
				IsFolder, IsSymLink bool
			}
		}
	}
	if err = readJSONResponse(response, http.MethodGet, endpoint, &out); err != nil {
		return err
	}
	if more || len(out.Changes) > MaxPRRepairFiles || len(out.ChangeCounts) > 16 {
		return ErrPRRepair
	}
	if !repairADOChangeCount(out.ChangeCounts, len(out.Changes)) {
		return ErrPRRepair
	}
	paths := make([]string, 0, len(out.Changes))
	for _, change := range out.Changes {
		if change.OriginalPath != "" || change.Item.IsFolder || change.Item.IsSymLink || change.Item.GitObjectType != "blob" || !strings.HasPrefix(change.Item.Path, "/") || (change.ChangeType != "edit" && change.ChangeType != "add" && change.ChangeType != "delete") {
			return ErrPRRepair
		}
		paths = append(paths, strings.TrimPrefix(change.Item.Path, "/"))
	}
	if !repairChangedPaths(value, paths) {
		return ErrPRRepair
	}
	return nil
}

func repairADOChangeCount(counts map[string]int, expected int) bool {
	count := 0
	for kind, n := range counts {
		if n < 0 || n > MaxPRRepairFiles || (n != 0 && kind != "Edit" && kind != "Add" && kind != "Delete") {
			return false
		}
		count += n
	}
	return count == expected
}
