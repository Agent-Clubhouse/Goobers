//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/childpublication"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationContainedParentDelegatesChildPublicationThroughRealWorkers(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_CHILD_KUBE_QUALIFICATION")
	for _, mode := range []string{"publication", "publication-lost-reply"} {
		t.Run(mode, func(t *testing.T) { qualifyContainedParentJourney(t, mode) })
	}
}

func configureParentPublicationQualification(t *testing.T, root string) {
	t.Helper()
	parentPath := filepath.Join(root, "config/gaggles/example/workflows/default-implement.yaml")
	parent := readFileContent(t, parentPath)
	parent = strings.Replace(parent, "allowPRPublication: false", "allowPRPublication: true", 1)
	parent = strings.ReplaceAll(parent, "[agent:model]", "[agent:model, repo:push, provider:pr:write]")
	parent = strings.ReplaceAll(parent, "[agent:model, repo:push]", "[agent:model, repo:push, provider:pr:write]")
	writeFileContent(t, parentPath, parent)
	gooberPath := filepath.Join(root, "config/gaggles/example/goobers/coder/goober.yaml")
	writeFileContent(t, gooberPath, strings.Replace(readFileContent(t, gooberPath), "    - repo:push\n", "    - repo:push\n    - provider:pr:write\n", 1))
	path := filepath.Join(root, "instance.yaml")
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(readFileContent(t, path)), &doc); err != nil {
		t.Fatal(err)
	}
	var grants []any
	for _, grant := range doc["credentials"].([]any) {
		key, _ := grant.(map[string]any)["capability"].(string)
		if key != "repo:push" && key != "provider:pr:write" {
			grants = append(grants, grant)
		}
	}
	for key, env := range map[string]string{"repo:push": "QUALIFICATION_PUBLICATION_PUSH_TOKEN", "provider:pr:write": "QUALIFICATION_PUBLICATION_PR_TOKEN"} {
		t.Setenv(env, "host-only-publication-"+key)
		grants = append(grants, map[string]any{"capability": key, "token": map[string]any{"env": env}})
	}
	doc["credentials"] = grants
	data, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	writeFileContent(t, path, string(data))
}

// Only the external Git remote and GitHub HTTPS endpoint are substituted.
// The actual publisher, credential broker and native provider adapter run.
type parentPublicationQualification struct {
	mu                            sync.Mutex
	remote, endpoint, head, child string
	creates                       int
	lostReply                     bool
}

func newParentPublicationQualification(t *testing.T, mode, source string) *parentPublicationQualification {
	t.Helper()
	if !strings.HasPrefix(mode, "publication") {
		return nil
	}
	p := &parentPublicationQualification{remote: filepath.Join(t.TempDir(), "forge.git"), lostReply: mode == "publication-lost-reply"}
	recoveryCLIGit(t, source, "init", "--bare", p.remote)
	recoveryCLIGit(t, source, "push", p.remote, "main")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if req.Header.Get("Authorization") != "Bearer host-only-publication-provider:pr:write" || req.URL.Path != "/repos/your-org/your-repo/pulls" {
			t.Error("publication request escaped its credential or repository boundary")
			http.Error(w, "unexpected request", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case http.MethodGet:
			if req.URL.Query().Get("head") != "your-org:"+p.head || req.URL.Query().Get("base") != "main" {
				t.Error("publication lookup changed the admitted branch")
			}
			_, _ = w.Write([]byte("[]"))
		case http.MethodPost:
			var body struct {
				Head, Base, Title, Body string
				Draft                   bool
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Head != p.head || body.Base != "main" || !body.Draft || !strings.Contains(body.Body, p.child) {
				t.Error("publication create changed its admitted request")
				http.Error(w, "invalid publication", http.StatusBadRequest)
				return
			}
			p.creates++
			if p.lostReply {
				// The external effect occurred; only its HTTP response is lost.
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close()
				return
			}
			_, _ = w.Write([]byte(`{"number":7,"html_url":"https://github.com/your-org/your-repo/pull/7","draft":true}`))
		default:
			t.Error("publication attempted an unexpected mutation")
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	p.endpoint = server.URL
	t.Cleanup(server.Close)
	return p
}

func (p *parentPublicationQualification) install(t *testing.T, service *daemonCredentialService) {
	t.Helper()
	if p == nil {
		return
	}
	service.childPublisher = func(target childpublication.Target, key string, credential httpapi.MintedCredential, scheme string) (childpublication.Publisher, error) {
		if credential.Value != "host-only-publication-"+key {
			return childpublication.Publisher{}, errors.New("host publication credential mismatch")
		}
		stage := &childStagePod{childPodFactory: childPodFactory{service: service}, identity: target.Identity}
		publisher, err := stage.publicationProvider(target, key, credential, scheme)
		if err != nil {
			return publisher, err
		}
		publisher.Git = hostPublicationGit{remote: p.remote}
		p.mu.Lock()
		p.head, p.child = target.Head, target.Child.RunID
		p.mu.Unlock()
		if publisher.PRs != nil {
			provider, ok := publisher.PRs.(*providers.GitHubProvider)
			if !ok {
				return publisher, errors.New("expected native GitHub provider")
			}
			provider.BaseURL = p.endpoint
		}
		return publisher, nil
	}
}

func (p *parentPublicationQualification) verify(t *testing.T, ctx context.Context, fixture pinnedChildFixture, queue *triggerqueue.Store, parent string, children []triggerqueue.ChildRecord) {
	t.Helper()
	if p == nil {
		return
	}
	p.mu.Lock()
	creates, head := p.creates, p.head
	p.mu.Unlock()
	if creates != 1 {
		t.Fatalf("PR creates = %d, want exactly one", creates)
	}
	if len(children) != 1 {
		t.Fatal("publication child identity lost")
	}
	if got := recoveryCLIGit(t, fixture.layout.Root, "--git-dir="+p.remote, "show", "refs/heads/"+head+":child-return.txt"); got != "published child return" {
		t.Fatal("publication lost child edits")
	}
	statuses, err := childpublication.Inspect(ctx, queue, children[0].Identity)
	if err != nil || len(statuses) != 2 {
		t.Fatal("publication custody missing", err)
	}
	wantPR := "confirmed"
	if p.lostReply {
		wantPR = "effect_pending"
	}
	if statuses[0].State != "confirmed" || statuses[1].State != wantPR || statuses[1].NeedsHuman != p.lostReply {
		t.Fatal("publication uncertainty lost", statuses)
	}
	if !p.lostReply && (statuses[1].PullRequestNumber != 7 || statuses[1].PullRequestURL != "https://github.com/your-org/your-repo/pull/7") {
		t.Fatal("confirmed PR link lost")
	}
	dir, err := fixture.layout.FindRunDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type != journal.EventArtifactRecorded || event.Ref == nil {
			continue
		}
		data, err := reader.ArtifactBytesBounded(*event.Ref, 128<<10)
		if err != nil {
			continue
		}
		var artifact struct {
			Completion runner.ChildHandoffCompletion `json:"completion"`
		}
		if json.Unmarshal(data, &artifact) != nil || len(artifact.Completion.Publications) != 2 {
			continue
		}
		for _, status := range artifact.Completion.Publications {
			if status.Action == "pr" && status.SourceRunID == children[0].RunID && status.State == wantPR && status.NeedsHuman == p.lostReply {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("parent did not retain child publication status")
	}
}
