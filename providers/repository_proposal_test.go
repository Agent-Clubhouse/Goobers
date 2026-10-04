package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type proposalTestProvider struct {
	t                          *testing.T
	kind                       ProviderKind
	proposal                   RepositoryProposal
	root, tree, commit, branch string
	posts                      []string
	prs                        []map[string]interface{}
	mode                       string
}

func newProposalTestProvider(t *testing.T, kind ProviderKind) (*proposalTestProvider, RepositoryProposalWriter) {
	t.Helper()
	id, digest := strings.Repeat("1", 32), strings.Repeat("2", 64)
	marker := RepositoryProposalMarker(id, digest)
	s := &proposalTestProvider{t: t, kind: kind, root: strings.Repeat("b", 40), tree: strings.Repeat("c", 40), commit: strings.Repeat("d", 40), proposal: RepositoryProposal{
		Repository: RepositoryRef{Provider: kind, Owner: "org", Name: "repo"}, CommandID: id, OperationDigest: digest, BaseBranch: "main", BaseCommit: strings.Repeat("a", 40), Path: "plan.md", PreviousBlob: sourceBlobID([]byte("old source")), Content: []byte("new source"), Message: "Update plan\n\n" + marker, Title: "Update plan", Body: "Proposed source edit.\n\n<!-- " + marker + " -->",
	}}
	client := &http.Client{Transport: roundTripFunc(s.roundTrip)}
	if kind == ProviderADO {
		s.proposal.Repository.Project = "project"
		return s, NewADOProvider("org", "project", "", func(p *ADOProvider) { p.Client = client })
	}
	return s, NewGitHubProvider("", WithHTTPClient(client))
}

func (s *proposalTestProvider) roundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != "GET" && r.Method != "POST" {
		s.t.Fatalf("unexpected mutation method %s", r.Method)
	}
	if r.Method == "POST" {
		s.posts = append(s.posts, r.URL.Path)
		if s.mode == "lost" {
			return nil, io.ErrUnexpectedEOF
		}
		if s.mode == "server failure" {
			return &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("uncertain"))}, nil
		}
		if s.mode == "oversized" {
			return &http.Response{StatusCode: 201, Header: http.Header{}, ContentLength: -1, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", MaxRepositorySourceResponseBytes+1)))}, nil
		}
	}
	if s.kind == ProviderADO {
		return s.ado(r)
	}
	return s.github(r)
}

func (s *proposalTestProvider) json(value interface{}) (*http.Response, error) {
	return sourceJSON(s.t, value), nil
}

func (s *proposalTestProvider) body(r *http.Request) map[string]interface{} {
	s.t.Helper()
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.t.Fatal(err)
	}
	return body
}

func (s *proposalTestProvider) github(r *http.Request) (*http.Response, error) {
	path := strings.TrimPrefix(r.URL.Path, "/repos/org/repo/")
	if r.Method == "POST" {
		return s.githubPost(r, path)
	}
	switch path {
	case "git/ref/heads/main":
		sha := s.proposal.BaseCommit
		if s.mode == "source moved" {
			sha = s.commit
		}
		return s.json(map[string]interface{}{"ref": "refs/heads/main", "object": map[string]string{"sha": sha}})
	case "git/ref/heads/" + RepositoryProposalBranch(s.proposal.CommandID):
		if s.branch == "" {
			return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("missing"))}, nil
		}
		return s.json(map[string]interface{}{"ref": "refs/heads/" + RepositoryProposalBranch(s.proposal.CommandID), "object": map[string]string{"sha": s.branch}})
	case "git/commits/" + s.proposal.BaseCommit:
		return s.json(map[string]interface{}{"sha": s.proposal.BaseCommit, "tree": map[string]string{"sha": s.root}})
	case "git/commits/" + s.commit:
		return s.json(s.githubCommit())
	case "git/trees/" + s.root, "git/trees/" + s.tree:
		tree, content := s.root, []byte("old source")
		if strings.HasSuffix(path, s.tree) {
			tree, content = s.tree, s.proposal.Content
		}
		mode := "100755"
		if s.mode == "symlink" {
			mode = "120000"
		}
		return s.json(map[string]interface{}{"sha": tree, "tree": []sourceTreeEntry{{Path: s.proposal.Path, Mode: mode, Type: "blob", SHA: sourceBlobID(content), Size: int64(len(content))}}})
	case "git/blobs/" + s.proposal.PreviousBlob:
		return s.githubBlob([]byte("old source"))
	case "git/blobs/" + sourceBlobID(s.proposal.Content):
		return s.githubBlob(s.proposal.Content)
	case "pulls":
		q := r.URL.Query()
		if q.Get("head") != "org:"+RepositoryProposalBranch(s.proposal.CommandID) || q.Get("base") != "main" || q.Get("state") != "all" || q.Get("per_page") != "2" {
			s.t.Fatal("unbounded PR observation", q)
		}
		return s.json(s.prs)
	default:
		s.t.Fatalf("unexpected GET %s", r.URL)
		return nil, errors.New("unexpected")
	}
}

func (s *proposalTestProvider) githubBlob(content []byte) (*http.Response, error) {
	return s.json(map[string]interface{}{"sha": sourceBlobID(content), "encoding": "base64", "size": len(content), "content": base64.StdEncoding.EncodeToString(content)})
}

func (s *proposalTestProvider) githubCommit() map[string]interface{} {
	parent := s.proposal.BaseCommit
	if s.mode == "wrong parent" {
		parent = s.tree
	}
	return map[string]interface{}{"sha": s.commit, "message": s.proposal.Message, "tree": map[string]string{"sha": s.tree}, "parents": []map[string]string{{"sha": parent}}}
}

func (s *proposalTestProvider) githubPost(r *http.Request, path string) (*http.Response, error) {
	body := s.body(r)
	switch path {
	case "git/trees":
		entries := body["tree"].([]interface{})
		entry := entries[0].(map[string]interface{})
		if body["base_tree"] != s.root || len(entries) != 1 || entry["path"] != s.proposal.Path || entry["content"] != string(s.proposal.Content) || entry["mode"] != "100755" || entry["type"] != "blob" {
			s.t.Fatal("unfaithful tree", body)
		}
		return s.json(map[string]string{"sha": s.tree})
	case "git/commits":
		parents := body["parents"].([]interface{})
		if body["tree"] != s.tree || body["message"] != s.proposal.Message || len(parents) != 1 || parents[0] != s.proposal.BaseCommit {
			s.t.Fatal("unpinned commit", body)
		}
		return s.json(s.githubCommit())
	case "git/refs":
		if body["ref"] != "refs/heads/"+RepositoryProposalBranch(s.proposal.CommandID) || body["sha"] != s.commit {
			s.t.Fatal("unsafe ref", body)
		}
		if s.branch != "" {
			return &http.Response{StatusCode: 422, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("exists"))}, nil
		}
		s.branch = s.commit
		return s.json(map[string]interface{}{"ref": body["ref"], "object": map[string]string{"sha": s.commit}})
	case "pulls":
		if body["draft"] != true || body["head"] != RepositoryProposalBranch(s.proposal.CommandID) || body["base"] != "main" || body["title"] != s.proposal.Title || body["body"] != s.proposal.Body {
			s.t.Fatal("unsafe PR", body)
		}
		pr := s.githubPR()
		s.prs = []map[string]interface{}{pr}
		return s.json(pr)
	default:
		s.t.Fatalf("unexpected POST %s", r.URL)
		return nil, errors.New("unexpected")
	}
}

func (s *proposalTestProvider) githubPR() map[string]interface{} {
	repo := map[string]string{"full_name": "org/repo"}
	return map[string]interface{}{"number": 7, "title": s.proposal.Title, "body": s.proposal.Body, "draft": true, "state": "open", "head": map[string]interface{}{"ref": RepositoryProposalBranch(s.proposal.CommandID), "sha": s.commit, "repo": repo}, "base": map[string]interface{}{"ref": "main", "repo": repo}}
}

func TestRepositoryProposalGitHubPhasesPreserveAncestryModeAndSeparateObservation(t *testing.T) {
	s, p := newProposalTestProvider(t, ProviderGitHub)
	in := RepositoryProposalPhaseInput{Phase: "tree", Proposal: s.proposal}
	for _, phase := range []string{"tree", "commit", "branch", "pull-request"} {
		in.Phase = phase
		before := len(s.posts)
		result, err := p.ApplyRepositoryProposalPhase(context.Background(), in)
		if err != nil || !result.MutationAttempted || !result.Acknowledged || len(s.posts) != before+1 {
			t.Fatalf("%s %+v %v", phase, result, err)
		}
		if result.TreeID != "" {
			in.TreeID = result.TreeID
		}
		if result.CommitID != "" {
			in.CommitID = result.CommitID
		}
		observation, err := p.ObserveRepositoryProposalPhase(context.Background(), in)
		if err != nil || !observation.Found || !observation.Matches || len(s.posts) != before+1 {
			t.Fatalf("observation %s %+v %v", phase, observation, err)
		}
	}
	s.mode = "source moved"
	s.prs[0]["state"], s.prs[0]["draft"] = "closed", false
	observed, err := p.ObserveRepositoryProposalPhase(context.Background(), in)
	if err != nil || !observed.Matches || observed.PullRequest.URL != "https://github.com/org/repo/pull/7" {
		t.Fatal(observed, err)
	}
	result, err := p.ApplyRepositoryProposalPhase(context.Background(), in)
	if err == nil || result.MutationAttempted {
		t.Fatal("new effect allowed after source move", result, err)
	}
}
