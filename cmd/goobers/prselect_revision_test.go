package main

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

func TestSelectedPRWorkspaceRevisionPreservesSource(t *testing.T) {
	base := providers.RepositoryRef{
		Provider: providers.ProviderGitHub, Owner: "org", Name: "app",
		ID: "1", URL: "https://github.example/org/app",
	}
	fork := base
	fork.Owner, fork.ID, fork.URL = "contributor", "2", "https://github.example/contributor/app"
	var sameRepoRevision apiv1.WorkspaceRevision
	for _, source := range []providers.RepositoryRef{base, fork} {
		pr := providers.PullRequestSummary{
			ID: "42", Number: 42, Head: "feature/same-name", Base: "main",
			HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
			HeadRepository: &source, BaseRepository: &base,
		}
		revision, err := selectedPRWorkspaceRevision(pr, providers.RepositoryRef{})
		if err != nil {
			t.Fatal(err)
		}
		if err := revision.Validate(); err != nil {
			t.Fatal(err)
		}
		if revision.Repository != source.RepositoryIdentity() || revision.CommitSHA != pr.HeadSHA ||
			revision.SourceRef != pr.Head || revision.SourceID != "42" ||
			revision.BaseRepository == nil || *revision.BaseRepository != base.RepositoryIdentity() ||
			revision.BaseSHA != pr.BaseSHA {
			t.Fatalf("revision lost selected snapshot: %+v", revision)
		}
		if source == base {
			sameRepoRevision = revision
			if revision.Repository != *revision.BaseRepository {
				t.Fatal("same-repository source and base identities differ")
			}
		} else if sameRepoRevision.Repository.CanonicalKey() == revision.Repository.CanonicalKey() {
			t.Fatal("same branch name conflated fork with base")
		}
	}
}

func TestSelectedPRWorkspaceRevisionRefusesMissingSource(t *testing.T) {
	base := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "org", Name: "app"}
	for _, pr := range []providers.PullRequestSummary{
		{Number: 42, Head: "feature", HeadSHA: strings.Repeat("a", 40)},
		{Number: 42, Head: "feature", HeadSHA: strings.Repeat("a", 40), HeadRepository: &providers.RepositoryRef{}},
		{Number: 42, Head: "feature", HeadRepository: &base},
	} {
		if _, err := selectedPRWorkspaceRevision(pr, base); err == nil {
			t.Fatalf("accepted incomplete source snapshot: %+v", pr)
		}
	}
}

func TestPRSelectResultIncludesExactWorkspaceRevision(t *testing.T) {
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(42, "Select exact source")
	headSHA, baseSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	server.addOpenPR(42, "goobers/implementation/exact", "main", headSHA, baseSHA, false, nil, nil)
	root := initDemo(t)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "revision-result")
	t.Chdir(t.TempDir())
	if code, stdout, stderr := runArgs(t, "pr-select", root); code != 0 {
		t.Fatalf("pr-select: code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	data, err := os.ReadFile("selected-pr.json")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Number            string                   `json:"number"`
		Head              string                   `json:"head"`
		HeadSHA           string                   `json:"headSha"`
		WorkspaceRevision *apiv1.WorkspaceRevision `json:"workspaceRevision"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Number != "42" || result.Head != "goobers/implementation/exact" || result.HeadSHA != headSHA {
		t.Fatalf("legacy outputs changed: %+v", result)
	}
	if result.WorkspaceRevision == nil {
		t.Fatal("typed workspace control missing")
	}
	revision := result.WorkspaceRevision
	if err := revision.Validate(); err != nil {
		t.Fatal(err)
	}
	if revision.CommitSHA != headSHA || revision.BaseSHA != baseSHA ||
		revision.Repository.ID != "1" || revision.Repository.Owner != "your-org" ||
		revision.BaseRepository == nil || revision.Repository != *revision.BaseRepository {
		t.Fatalf("wrong selected revision: %+v", revision)
	}
}

func TestADOSelectionUsesOneSourceSnapshot(t *testing.T) {
	base := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "app", ID: "1", URL: "https://ado.example"}
	fork := base
	fork.Project, fork.ID = "fork-project", "2"
	poll := providers.PullRequestPollResult{
		Number: 42, State: "open", HeadBranch: "goobers/implementation/same-name", BaseBranch: "main",
		HeadSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("c", 40),
		HeadRepository: &fork, BaseRepository: &base, CheckState: providers.CheckStatePassing,
	}
	provider := &fakeADOSelectProvider{
		open: []providers.PullRequestSummary{{
			Number: 42, Head: poll.HeadBranch, Base: "main",
			HeadSHA: strings.Repeat("a", 40), HeadRepository: &base,
		}},
		polls: map[int]providers.PullRequestPollResult{42: poll},
	}
	listed, _, err := pullRequestsForSelectionADO(context.Background(), provider, base, "main",
		[]string{"goobers/implementation/"}, authorScopeGoobers,
		providers.ListPullRequestsRequest{}, "", prSelectCompleteSnapshot, "")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list selection: %+v, %v", listed, err)
	}
	targeted, err := adoSelectionCandidate(context.Background(), provider, base, "42")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(listed[0].HeadRepository, targeted.HeadRepository) ||
		listed[0].HeadSHA != targeted.HeadSHA || listed[0].HeadSHA != poll.HeadSHA ||
		listed[0].HeadRepository.ID != "2" {
		t.Fatalf("mixed list identity and polled SHA: %+v / %+v", listed[0], targeted)
	}
}
