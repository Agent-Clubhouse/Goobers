package providers

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// GitHub's native expected-head mutation atomically creates one descendant and
// advances its selected branch. REST update-ref(force=false) is not an exact CAS.
// https://docs.github.com/en/graphql/reference/commits#createcommitonbranch
const repairGitHubCommitMutation = `mutation($input:CreateCommitOnBranchInput!){createCommitOnBranch(input:$input){commit{oid message parents(first:2){nodes{oid}}} ref{name prefix target{oid}}}}`

// ApplyPullRequestRepair sends one non-retrying exact-head commit mutation.
func (p *GitHubProvider) ApplyPullRequestRepair(ctx context.Context, value PullRequestRepair) (PRRepairResult, error) {
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
	additions := []map[string]string{}
	deletions := []map[string]string{}
	for _, change := range value.Changes {
		if change.Content == nil {
			deletions = append(deletions, map[string]string{"path": change.Path})
		} else {
			additions = append(additions, map[string]string{"path": change.Path, "contents": base64.StdEncoding.EncodeToString([]byte(*change.Content))})
		}
	}
	headline, body, _ := strings.Cut(value.Message, "\n\n")
	input := map[string]interface{}{"branch": map[string]string{"repositoryNameWithOwner": value.Target.Repository.Owner + "/" + value.Target.Repository.Name, "branchName": value.Target.Head}, "expectedHeadOid": value.Target.HeadSHA, "message": map[string]string{"headline": headline, "body": body}, "fileChanges": map[string]interface{}{"additions": additions, "deletions": deletions}, "clientMutationId": value.CommandID}
	var out struct {
		CreateCommitOnBranch struct {
			Commit struct {
				OID, Message string
				Parents      struct{ Nodes []struct{ OID string } }
			}
			Ref struct {
				Name, Prefix string
				Target       struct{ OID string }
			}
		}
	}
	result.MutationAttempted = true
	if err := p.graphql(WithoutMutationRetries(ctx), repairGitHubCommitMutation, map[string]interface{}{"input": input}, &out); err != nil {
		return result, err
	}
	commit, ref := out.CreateCommitOnBranch.Commit, out.CreateCommitOnBranch.Ref
	if !ValidSourceCommit(commit.OID) || len(commit.Parents.Nodes) != 1 || commit.Parents.Nodes[0].OID != value.Target.HeadSHA || strings.TrimSuffix(commit.Message, "\n") != strings.TrimSuffix(value.Message, "\n") || ref.Name != value.Target.Head || ref.Prefix != "refs/heads/" || ref.Target.OID != commit.OID {
		return result, ErrPRRepair
	}
	result.Acknowledged, result.CommitID = true, commit.OID
	p.recordExternalRef(ctx, ExternalRef{Provider: ProviderGitHub, Ref: value.Target.Repository.Owner + "/" + value.Target.Repository.Name + "/pull/" + value.Target.ID, URL: value.Target.URL, Operation: "interactive-repair"})
	return result, nil
}

// ObservePullRequestRepair verifies exact command, ancestry and file delta.
func (p *GitHubProvider) ObservePullRequestRepair(ctx context.Context, value PullRequestRepair) (PRRepairObservation, error) {
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
	if err = p.verifyRepairCommit(ctx, value, current.HeadSHA); err != nil {
		return result, err
	}
	if err = verifyRepairFiles(ctx, value, current.HeadSHA, p.repairFile); err != nil {
		return result, err
	}
	return PRRepairObservation{Matches: true, CommitID: current.HeadSHA}, nil
}
func (p *GitHubProvider) verifyRepairCommit(ctx context.Context, value PullRequestRepair, sha string) error {
	repo := value.Target.Repository
	endpoint, err := joinURL(p.BaseURL, "repos", repo.Owner, repo.Name, "commits", sha)
	if err != nil {
		return err
	}
	endpoint, err = addQuery(endpoint, url.Values{"per_page": {strconv.Itoa(MaxPRRepairFiles + 1)}})
	if err != nil {
		return err
	}
	response, err := p.send(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	more := response.Header.Get("Link") != ""
	var out struct {
		SHA     string
		Commit  struct{ Message string }
		Parents []struct{ SHA string }
		Files   []struct {
			Filename, Status string
			PreviousFilename string `json:"previous_filename"`
		}
	}
	if err = readJSONResponse(response, http.MethodGet, endpoint, &out); err != nil {
		return err
	}
	if more || out.SHA != sha || len(out.Parents) != 1 || out.Parents[0].SHA != value.Target.HeadSHA || strings.TrimSuffix(out.Commit.Message, "\n") != strings.TrimSuffix(value.Message, "\n") || len(out.Files) > MaxPRRepairFiles {
		return ErrPRRepair
	}
	paths := make([]string, 0, len(value.Changes))
	for _, file := range out.Files {
		if file.Status == "renamed" {
			if file.PreviousFilename == "" {
				return ErrPRRepair
			}
			paths = append(paths, file.PreviousFilename, file.Filename)
			continue
		}
		if file.PreviousFilename != "" || (file.Status != "added" && file.Status != "modified" && file.Status != "removed") {
			return ErrPRRepair
		}
		paths = append(paths, file.Filename)
	}
	if !repairChangedPaths(value, paths) {
		return ErrPRRepair
	}
	return nil
}
