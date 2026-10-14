package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const adoZeroObjectIDForTest = "0000000000000000000000000000000000000000"

// fakeADORefs is the Git Refs and pull-request list surface of one fake Azure
// DevOps repository: branch tips by name, the source branches of open pull
// requests by target branch, and every ref update (POST refs) it received.
type fakeADORefs struct {
	mu sync.Mutex
	// tips maps a branch name (without refs/heads/) to its tip.
	tips map[string]string
	// stackedOn maps a branch name to the source branch of an open pull
	// request that targets it.
	stackedOn map[string]string
	// staleOnDelete makes a ref update fail as staleOldObjectId and, when
	// goneAfterStale is set, removes the ref first, modelling Azure DevOps'
	// own deletion landing between the read and the delete.
	staleOnDelete  bool
	goneAfterStale bool
	updates        []map[string]string
	auths          []string
}

func newFakeADORefs(tips map[string]string) *fakeADORefs {
	return &fakeADORefs{tips: tips, stackedOn: map[string]string{}}
}

// register mounts the refs and pull-request list endpoints of org/project/repo.
func (f *fakeADORefs) register(t *testing.T, mux *http.ServeMux, org, project, repo string) {
	t.Helper()
	f.registerRefs(t, mux, org, project, repo)
	mux.HandleFunc("/"+org+"/"+project+"/_apis/git/repositories/"+repo+"/pullrequests", f.servePullRequests)
}

// registerRefs mounts only the refs endpoint, for a fixture that serves its
// own pull-request list.
func (f *fakeADORefs) registerRefs(t *testing.T, mux *http.ServeMux, org, project, repo string) {
	t.Helper()
	mux.HandleFunc("/"+org+"/"+project+"/_apis/git/repositories/"+repo+"/refs", f.serveRefs(t))
}

func (f *fakeADORefs) serveRefs(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		switch r.Method {
		case http.MethodGet:
			prefix := strings.TrimPrefix(r.URL.Query().Get("filter"), "heads/")
			refs := []map[string]string{}
			for name, sha := range f.tips {
				if strings.HasPrefix(name, prefix) {
					refs = append(refs, map[string]string{"name": "refs/heads/" + name, "objectId": sha})
				}
			}
			writeFakeJSON(w, map[string]any{"value": refs})
		case http.MethodPost:
			var body []map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 {
				t.Errorf("ref update body = %v (%v), want one update", body, err)
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			update := body[0]
			f.updates = append(f.updates, update)
			name := strings.TrimPrefix(update["name"], "refs/heads/")
			tip, exists := f.tips[name]
			if f.staleOnDelete || !exists || tip != update["oldObjectId"] {
				if f.goneAfterStale {
					delete(f.tips, name)
				}
				writeFakeJSON(w, map[string]any{"value": []map[string]any{{"name": update["name"], "success": false, "updateStatus": "staleOldObjectId"}}})
				return
			}
			delete(f.tips, name)
			writeFakeJSON(w, map[string]any{"value": []map[string]any{{"name": update["name"], "success": true, "updateStatus": "succeeded"}}})
		default:
			t.Errorf("unexpected %s on refs", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}
}

func (f *fakeADORefs) servePullRequests(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := strings.TrimPrefix(r.URL.Query().Get("searchCriteria.targetRefName"), "refs/heads/")
	prs := []map[string]any{}
	for base, source := range f.stackedOn {
		if (target == "" || target == base) && r.URL.Query().Get("$skip") == "0" {
			prs = append(prs, map[string]any{
				"pullRequestId": 77, "status": "active",
				"sourceRefName": "refs/heads/" + source,
				"targetRefName": "refs/heads/" + base,
			})
		}
	}
	writeFakeJSON(w, map[string]any{"value": prs})
}

func (f *fakeADORefs) snapshot() (map[string]string, []map[string]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tips := make(map[string]string, len(f.tips))
	for k, v := range f.tips {
		tips[k] = v
	}
	return tips, append([]map[string]string(nil), f.updates...), append([]string(nil), f.auths...)
}

// adoTerminalConfig is an instance whose only repository is on Azure DevOps,
// authenticated as kind.
func adoTerminalConfig(kind string) *instance.Config {
	repo := instance.RepoRef{
		Provider: string(providers.ProviderADO),
		Owner:    "example-org",
		Project:  "example-project",
		Name:     "example-repo",
		Auth:     &instance.RepoAuthConfig{Kind: kind},
	}
	if kind == instance.ADOAuthPAT {
		repo.Token = instance.TokenRef{Env: "TERMINAL_ADO_PAT"}
	}
	return &instance.Config{Repos: []instance.RepoRef{repo}}
}

// pointTerminalADODeleterAt makes the production ADO arm talk to serverURL.
// The real constructor still builds the provider and its guard; only the
// host changes.
func pointTerminalADODeleterAt(t *testing.T, serverURL string) {
	t.Helper()
	previous := newADOTerminalBranchDeleter
	newADOTerminalBranchDeleter = func(repo providers.RepositoryRef, kind string, source providers.TokenSource, registrar providers.SecretRegistrar) (providers.BranchDeleter, error) {
		deleter, err := previous(repo, kind, source, registrar)
		if err != nil {
			return nil, err
		}
		guarded, ok := deleter.(adoUnusedBranchDeleter)
		if !ok {
			t.Fatalf("ADO terminal deleter = %T, want the in-use guard", deleter)
		}
		guarded.BaseURL = serverURL
		return guarded, nil
	}
	t.Cleanup(func() { newADOTerminalBranchDeleter = previous })
}

// TestBuildTerminalBranchDeleteDeletesADORunBranch is #5957's terminal arm: an
// ADO run's leftover branch is deleted through the refs update API, with the
// token materialized for github:branch:delete sent in the repository's own
// scheme (Basic for a PAT), and the delete conditional on the tip it read.
func TestBuildTerminalBranchDeleteDeletesADORunBranch(t *testing.T) {
	const branch = "goobers/implementation/terminal-run"
	const tip = "3333333333333333333333333333333333333333"
	t.Setenv("TERMINAL_ADO_PAT", "terminal-ado-pat")
	refs := newFakeADORefs(map[string]string{branch: tip})
	mux := http.NewServeMux()
	refs.register(t, mux, "example-org", "example-project", "example-repo")
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	pointTerminalADODeleterAt(t, server.URL)

	deleteBranch, repo, err := buildTerminalBranchDelete(adoTerminalConfig(instance.ADOAuthPAT), apiv1.RepoRef{}, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantRepo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "example-org", Project: "example-project", Name: "example-repo"}
	if repo != wantRepo {
		t.Fatalf("terminal repo = %+v, want %+v (the ADO project is part of the address)", repo, wantRepo)
	}
	result, err := deleteBranch(context.Background(), providers.DeleteBranchRequest{Repository: repo, Name: branch})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Deleted {
		t.Fatalf("result = %+v, want deleted", result)
	}
	tips, updates, auths := refs.snapshot()
	if _, kept := tips[branch]; kept {
		t.Fatalf("branch %q survived terminal cleanup", branch)
	}
	if len(updates) != 1 || updates[0]["oldObjectId"] != tip || updates[0]["newObjectId"] != adoZeroObjectIDForTest {
		t.Fatalf("ref updates = %v, want one delete conditional on %s", updates, tip)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:terminal-ado-pat"))
	for _, auth := range auths {
		if auth != wantAuth {
			t.Fatalf("Authorization = %q, want the materialized PAT as Basic", auth)
		}
	}
}

// TestBuildTerminalBranchDeleteKeepsADOBranchesInUse: the terminal arm keeps
// a branch an open pull request uses, as the base of a stacked pull request
// (the merge path's guard, PRL-072) or as its source (a pull request a resumed
// or continued run, or a person, opened from it), and journals why.
func TestBuildTerminalBranchDeleteKeepsADOBranchesInUse(t *testing.T) {
	const branch = "goobers/implementation/terminal-run"
	for _, tc := range []struct {
		name       string
		base, head string
		wantReason string
	}{
		{name: "stacked", base: branch, head: "goobers/implementation/child", wantReason: "stacked-pull-request-open"},
		{name: "source of an open pull request", base: "main", head: branch, wantReason: "pull-request-open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERMINAL_ADO_PAT", "terminal-ado-pat")
			refs := newFakeADORefs(map[string]string{branch: "4444444444444444444444444444444444444444"})
			refs.stackedOn[tc.base] = tc.head
			mux := http.NewServeMux()
			refs.register(t, mux, "example-org", "example-project", "example-repo")
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			pointTerminalADODeleterAt(t, server.URL)

			deleteBranch, repo, err := buildTerminalBranchDelete(adoTerminalConfig(instance.ADOAuthPAT), apiv1.RepoRef{}, journal.NewRegistryScrubber(), nil)
			if err != nil {
				t.Fatal(err)
			}
			runsDir, runID, jr := newTerminalBranchJournal(t, true, false)
			if err := finalizeTerminalBranch(runsDir, runID, jr, repo, func(ctx context.Context, req providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
				req.Name = branch
				return deleteBranch(ctx, req)
			}); err != nil {
				t.Fatal(err)
			}
			events := terminalBranchCleanupEvents(t, runsDir, runID)
			if len(events) != 1 || events[0].Runner["outcome"] != branchCleanupSkipped || events[0].Runner["reason"] != tc.wantReason {
				t.Fatalf("cleanup events = %+v, want skipped/%s", events, tc.wantReason)
			}
			tips, updates, _ := refs.snapshot()
			if _, kept := tips[branch]; !kept || len(updates) != 0 {
				t.Fatalf("tips = %v, updates = %v, want the branch untouched", tips, updates)
			}
		})
	}
}

// TestTerminalADOCredentialKindFollowsRepositoryAuth: the terminal arm sends
// the materialized token in the repository's configured scheme — a PAT as
// Basic, an Entra-backed kind as Bearer — and refuses a repository it cannot
// resolve rather than guessing.
func TestTerminalADOCredentialKindFollowsRepositoryAuth(t *testing.T) {
	for auth, want := range map[string]string{
		instance.ADOAuthPAT:              providers.ADOCredentialKindPAT,
		instance.ADOAuthAzureCLI:         providers.ADOCredentialKindBearer,
		instance.ADOAuthWorkloadIdentity: providers.ADOCredentialKindBearer,
		instance.ADOAuthManagedIdentity:  providers.ADOCredentialKindBearer,
	} {
		got, err := terminalADOCredentialKind(adoTerminalConfig(auth), apiv1.RepoRef{})
		if err != nil || got != want {
			t.Fatalf("%s: kind = %q, %v; want %q", auth, got, err, want)
		}
	}
	unconfigured := apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "other-org", Project: "p", Name: "other-repo"}
	if _, err := terminalADOCredentialKind(adoTerminalConfig(instance.ADOAuthPAT), unconfigured); err == nil {
		t.Fatal("an unconfigured terminal project resolved a credential kind")
	}
}
