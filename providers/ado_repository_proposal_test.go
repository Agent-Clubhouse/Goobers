package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func (s *proposalTestProvider) ado(r *http.Request) (*http.Response, error) {
	path := strings.TrimPrefix(r.URL.Path, "/org/project/_apis/git/repositories/repo/")
	if r.Method == "POST" {
		return s.adoPost(r, path)
	}
	switch path {
	case "refs":
		name := r.URL.Query().Get("filter")
		var refs []map[string]string
		if name == "heads/main" {
			sha := s.proposal.BaseCommit
			if s.mode == "source moved" {
				sha = s.commit
			}
			refs = []map[string]string{{"name": "refs/heads/main", "objectId": sha}}
		} else if name == "heads/"+RepositoryProposalBranch(s.proposal.CommandID) {
			if s.branch != "" {
				refs = []map[string]string{{"name": "refs/" + name, "objectId": s.branch}}
			}
		} else {
			s.t.Fatal("unscoped refs", r.URL)
		}
		return s.json(map[string]interface{}{"value": refs})
	case "items":
		q := r.URL.Query()
		if q.Get("path") != "/plan.md" || q.Get("versionDescriptor.versionType") != "commit" || q.Get("resolveLfs") != "false" {
			s.t.Fatal("unpinned item", q)
		}
		commit := q.Get("versionDescriptor.version")
		content := []byte("old source")
		if commit == s.commit {
			content = s.proposal.Content
		} else if commit != s.proposal.BaseCommit {
			s.t.Fatal("unexpected commit", commit)
		}
		return s.json(map[string]interface{}{"commitId": commit, "objectId": sourceBlobID(content), "path": "/plan.md", "gitObjectType": "blob", "content": string(content), "isSymLink": s.mode == "symlink"})
	case "commits/" + s.commit:
		return s.json(s.adoCommit())
	case "commits/" + s.commit + "/changes":
		if r.URL.Query().Get("top") != "2" || r.URL.Query().Get("skip") != "0" {
			s.t.Fatal("unbounded commit changes", r.URL)
		}
		count := 1
		if s.mode == "extra changes" {
			count = 2
		}
		return s.json(map[string]interface{}{"changeCounts": map[string]int{"Edit": count}, "changes": []map[string]interface{}{{"changeType": "edit", "item": map[string]interface{}{"path": "/plan.md", "gitObjectType": "blob"}}}})
	case "pullrequests":
		q := r.URL.Query()
		if q.Get("searchCriteria.sourceRefName") != "refs/heads/"+RepositoryProposalBranch(s.proposal.CommandID) || q.Get("searchCriteria.targetRefName") != "refs/heads/main" || q.Get("searchCriteria.status") != "all" || q.Get("$top") != "2" {
			s.t.Fatal("unbounded PR query", q)
		}
		return s.json(map[string]interface{}{"value": s.prs})
	default:
		s.t.Fatalf("unexpected GET %s", r.URL)
		return nil, errors.New("unexpected")
	}
}

func (s *proposalTestProvider) adoPost(r *http.Request, path string) (*http.Response, error) {
	name := "refs/heads/" + RepositoryProposalBranch(s.proposal.CommandID)
	if path == "refs" {
		var refs []map[string]string
		if err := json.NewDecoder(r.Body).Decode(&refs); err != nil {
			s.t.Fatal(err)
		}
		if len(refs) != 1 || refs[0]["name"] != name || refs[0]["oldObjectId"] != adoZeroObjectID || refs[0]["newObjectId"] != s.proposal.BaseCommit {
			s.t.Fatal("not absent-only creation", refs)
		}
		success, status := true, "succeeded"
		if s.mode == "stale ref" || s.branch != "" {
			success, status = false, "staleOldObjectId"
		} else {
			s.branch = s.proposal.BaseCommit
		}
		return s.json(map[string]interface{}{"value": []proposalADORefResult{{Name: name, OldObjectID: adoZeroObjectID, NewObjectID: s.proposal.BaseCommit, Success: success, UpdateStatus: status}}})
	}
	body := s.body(r)
	switch path {
	case "pushes":
		refs := body["refUpdates"].([]interface{})
		ref := refs[0].(map[string]interface{})
		commits := body["commits"].([]interface{})
		commit := commits[0].(map[string]interface{})
		changes := commit["changes"].([]interface{})
		change := changes[0].(map[string]interface{})
		if len(refs) != 1 || ref["name"] != name || ref["oldObjectId"] != s.proposal.BaseCommit || len(commits) != 1 || commit["comment"] != s.proposal.Message || len(changes) != 1 || change["changeType"] != "edit" || change["item"].(map[string]interface{})["path"] != "/plan.md" || change["newContent"].(map[string]interface{})["content"] != string(s.proposal.Content) {
			s.t.Fatal("unpinned push", body)
		}
		s.branch = s.commit
		return s.json(map[string]interface{}{"commits": []map[string]interface{}{s.adoCommit()}, "refUpdates": []proposalADORefResult{{Name: name, OldObjectID: s.proposal.BaseCommit, NewObjectID: s.commit}}})
	case "pullrequests":
		if body["sourceRefName"] != name || body["targetRefName"] != "refs/heads/main" || body["title"] != s.proposal.Title || body["description"] != s.proposal.Body || body["isDraft"] != true {
			s.t.Fatal("unsafe PR", body)
		}
		pr := s.adoPR()
		s.prs = []map[string]interface{}{pr}
		return s.json(pr)
	default:
		s.t.Fatalf("unexpected POST %s", r.URL)
		return nil, errors.New("unexpected")
	}
}

func (s *proposalTestProvider) adoCommit() map[string]interface{} {
	parent := s.proposal.BaseCommit
	if s.mode == "wrong parent" {
		parent = s.tree
	}
	return map[string]interface{}{"commitId": s.commit, "treeId": s.tree, "comment": s.proposal.Message + "\n", "parents": []string{parent}}
}

func (s *proposalTestProvider) adoPR() map[string]interface{} {
	return map[string]interface{}{"pullRequestId": 7, "title": s.proposal.Title, "description": s.proposal.Body, "isDraft": true, "status": "active", "sourceRefName": "refs/heads/" + RepositoryProposalBranch(s.proposal.CommandID), "targetRefName": "refs/heads/main", "lastMergeSourceCommit": map[string]string{"commitId": s.commit}, "repository": map[string]interface{}{"name": "repo", "project": map[string]string{"name": "project"}}}
}

func TestRepositoryProposalADOPhasesRequireAbsentRefAndPinnedSingleEdit(t *testing.T) {
	s, p := newProposalTestProvider(t, ProviderADO)
	in := RepositoryProposalPhaseInput{Phase: "branch", Proposal: s.proposal}
	result, err := p.ApplyRepositoryProposalPhase(context.Background(), in)
	if err != nil || !result.Acknowledged || result.CommitID != s.proposal.BaseCommit || len(s.posts) != 1 {
		t.Fatal(result, err)
	}
	observed, err := p.ObserveRepositoryProposalPhase(context.Background(), in)
	if err != nil || !observed.Matches || len(s.posts) != 1 {
		t.Fatal(observed, err)
	}
	in.Phase = "commit"
	result, err = p.ApplyRepositoryProposalPhase(context.Background(), in)
	if err != nil || !result.Acknowledged || result.CommitID != s.commit || len(s.posts) != 2 {
		t.Fatal(result, err)
	}
	// A lost push receipt can be observed by exact branch, one parent, marker and
	// file bytes. Observation still makes no transport acknowledgement claim.
	observed, err = p.ObserveRepositoryProposalPhase(context.Background(), in)
	if err != nil || !observed.Matches || observed.CommitID != s.commit || len(s.posts) != 2 {
		t.Fatal(observed, err)
	}
	in.Phase, in.CommitID = "pull-request", s.commit
	result, err = p.ApplyRepositoryProposalPhase(context.Background(), in)
	if err != nil || !result.Acknowledged || result.PullRequest == nil || len(s.posts) != 3 {
		t.Fatal(result, err)
	}
	s.mode = "source moved"
	s.prs[0]["status"], s.prs[0]["isDraft"] = "completed", false
	observed, err = p.ObserveRepositoryProposalPhase(context.Background(), in)
	if err != nil || !observed.Matches || len(s.posts) != 3 || observed.PullRequest.URL != "https://dev.azure.com/org/project/_git/repo/pullrequest/7" {
		t.Fatal(observed, err)
	}
}

func TestRepositoryProposalADORefFailureIsNotAcknowledged(t *testing.T) {
	s, p := newProposalTestProvider(t, ProviderADO)
	s.mode = "stale ref"
	result, err := p.ApplyRepositoryProposalPhase(context.Background(), RepositoryProposalPhaseInput{Phase: "branch", Proposal: s.proposal})
	if !errors.Is(err, ErrRepositoryProposal) || !result.MutationAttempted || result.Acknowledged || len(s.posts) != 1 {
		t.Fatal(result, err)
	}
}
