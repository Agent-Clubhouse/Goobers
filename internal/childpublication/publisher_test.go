package childpublication

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

func publicationFixture(t *testing.T) (*triggerqueue.Store, Target, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "queue.db")
	q, err := triggerqueue.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	id := triggerqueue.ChildIdentity{ChildParent: triggerqueue.ChildParent{Gaggle: "own", ParentRunID: strings.Repeat("a", 32)}, StageOccurrence: "plan/1", InvocationKey: "publish"}
	c, _, err := q.AcceptChild(t.Context(), triggerqueue.ChildAcceptance{Identity: id, Actor: "parent-stage", Payload: []byte(`{}`), MaxChildren: 1}, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	digest := journal.Digest([]byte("source"))
	lineage := &journal.ChildLineage{Gaggle: id.Gaggle, ParentRunID: id.ParentRunID, ParentWorkflow: "parent", StageOccurrence: id.StageOccurrence, InvocationKey: id.InvocationKey, AcceptanceID: c.AcceptanceID, SourceDigest: digest, EnvelopeDigest: digest}
	run := journal.RunIdentity{RunID: c.RunID, Gaggle: id.Gaggle, Child: lineage, ConfigGeneration: digest, WorkflowDigest: digest, GooberDigest: digest}
	return q, Target{Child: c, Identity: run, Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web", Project: "project"}, Remote: "https://github.com/acme/web.git", Base: "main", Head: "factory/children/" + c.RunID, Stage: "push", Workspace: "owned"}, path
}

type publicationGitFake struct{ sha string }

func (g publicationGitFake) Head(context.Context, string, string, string) (string, error) {
	return g.sha, nil
}
func (publicationGitFake) Create(context.Context, string, string, string, string) error {
	return errors.New("unexpected push")
}

type publicationHTTP struct {
	t                       *testing.T
	target                  Target
	ado, exists, hide, lose bool
	creates, patches        int
}

func (h *publicationHTTP) Do(r *http.Request) (*http.Response, error) {
	h.t.Helper()
	if r.Header.Get("Authorization") == "" {
		h.t.Fatal("provider authorization missing")
	}
	result := `{"number":7,"html_url":"https://github.com/acme/web/pull/7"}`
	if h.ado {
		result = `{"pullRequestId":7,"title":"child title","sourceRefName":"refs/heads/` + h.target.Head + `","targetRefName":"refs/heads/main","status":"active","repository":{"name":"web","project":{"name":"project"}},"_links":{"web":{"href":"https://dev.azure.com/acme/project/_git/web/pullrequest/7"}}}`
	}
	response := result
	switch r.Method {
	case http.MethodGet:
		if !h.ado && (r.URL.Query().Get("head") != "acme:"+h.target.Head || r.URL.Query().Get("base") != "main") {
			h.t.Fatal("wrong native PR selector", r.URL)
		}
		values := "[]"
		if h.exists && !h.hide {
			values = "[" + result + "]"
		}
		response = values
		if h.ado {
			response = `{"value":` + values + `}`
		}
	case http.MethodPost:
		h.creates++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			h.t.Fatal(err)
		}
		head, base := body["head"], body["base"]
		if h.ado {
			head, base = body["sourceRefName"], body["targetRefName"]
		}
		wantHead, wantBase := h.target.Head, "main"
		if h.ado {
			wantHead, wantBase = "refs/heads/"+wantHead, "refs/heads/main"
		}
		if head != wantHead || base != wantBase || body["title"] != "child title" {
			h.t.Fatal("provider request widened", body)
		}
		h.exists = true
		if h.lose {
			return nil, errors.New("lost create response")
		}
	case http.MethodPatch:
		h.patches++
	default:
		h.t.Fatal("unexpected provider effect", r.Method, r.URL)
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
}

func confirmedPublicationBranch(t *testing.T, q *triggerqueue.Store, target Target) string {
	t.Helper()
	sha := strings.Repeat("b", 40)
	intent, _ := json.Marshal(BranchIntent{Version: 1, RunID: target.Identity.RunID, Lineage: *target.Identity.Child, Stage: target.Stage, Repository: target.Repository, Remote: target.Remote, Base: target.Base, Head: target.Head, Commit: sha})
	rec, err := q.PrepareChildPublication(t.Context(), target.Child.Identity, "branch", intent)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.BeginChildPublicationEffect(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	receipt, _ := json.Marshal(BranchReceipt{Head: target.Head, SHA: sha})
	if err = q.ConfirmChildPublication(t.Context(), rec, receipt); err != nil {
		t.Fatal(err)
	}
	return sha
}

func TestPublicationNativeProvidersReconcileLostResponseWithoutDuplicate(t *testing.T) {
	for _, ado := range []bool{false, true} {
		name := "github"
		if ado {
			name = "ado"
		}
		t.Run(name, func(t *testing.T) {
			q, target, _ := publicationFixture(t)
			if ado {
				target.Repository.Provider = providers.ProviderADO
			}
			sha := confirmedPublicationBranch(t, q, target)
			httpFake := &publicationHTTP{t: t, target: target, ado: ado, lose: true}
			p := Publisher{Queue: q, Git: publicationGitFake{sha: sha}}
			if ado {
				p.PRs = providers.NewADOProvider("acme", "project", "host-token", func(p *providers.ADOProvider) { p.Client = httpFake }, providers.WithADOMaxRateLimitRetries(0))
			} else {
				p.PRs = providers.NewGitHubProvider("host-token", providers.WithHTTPClient(httpFake), providers.WithMaxTransientRetries(0))
			}
			if _, err := p.OpenPR(t.Context(), target, "child title", "body", true); err == nil {
				t.Fatal("lost reply reported confirmed")
			}
			rec, err := q.ChildPublication(t.Context(), target.Child.Identity, "pr")
			if err != nil || rec.State != "effect_pending" {
				t.Fatal(rec, err)
			}
			// Disappearing/closed PRs after an uncertain create must never produce a new POST.
			httpFake.hide = true
			if _, err = p.OpenPR(t.Context(), target, "child title", "body", true); err == nil {
				t.Fatal("missing outcome treated as absent effect")
			}
			httpFake.hide = false
			result, err := p.OpenPR(t.Context(), target, "child title", "body", true)
			if err != nil || result.Number != 7 {
				t.Fatal(result, err)
			}
			if _, err = p.OpenPR(t.Context(), target, "changed title", "body", true); !errors.Is(err, triggerqueue.ErrConflict) {
				t.Fatal("changed intent accepted", err)
			}
			if _, err = p.OpenPR(t.Context(), target, "child title", "body", true); err != nil {
				t.Fatal(err)
			}
			if httpFake.creates != 1 || httpFake.patches != 0 {
				t.Fatal("recovery repeated native effect", httpFake.creates, httpFake.patches)
			}
		})
	}
}

func TestPublicationPRRefusesForeignPRAndChangedHead(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "head drift", true: "foreign PR"}[foreign], func(t *testing.T) {
			q, target, _ := publicationFixture(t)
			sha := confirmedPublicationBranch(t, q, target)
			fake := &publicationHTTP{t: t, target: target, exists: foreign}
			if !foreign {
				sha = strings.Repeat("c", 40)
			}
			p := Publisher{Queue: q, Git: publicationGitFake{sha: sha}, PRs: providers.NewGitHubProvider("token", providers.WithHTTPClient(fake), providers.WithMaxTransientRetries(0))}
			if _, err := p.OpenPR(t.Context(), target, "child title", "body", true); err == nil {
				t.Fatal("foreign effect adopted")
			}
			if fake.creates != 0 || fake.patches != 0 {
				t.Fatal("unsafe native effect")
			}
		})
	}
}

type concurrentPublicationPR struct {
	arrived chan struct{}
	release chan struct{}
	creates atomic.Int32
}

func (p *concurrentPublicationPR) FindPullRequestByBranch(context.Context, providers.RepositoryRef, string, string) (providers.PullRequestResult, bool, error) {
	p.arrived <- struct{}{}
	<-p.release
	return providers.PullRequestResult{}, false, nil
}
func (p *concurrentPublicationPR) OpenPullRequest(context.Context, providers.PullRequestRequest) (providers.PullRequestResult, error) {
	p.creates.Add(1)
	return providers.PullRequestResult{ID: "7", Number: 7, URL: "https://github.com/acme/web/pull/7"}, nil
}
func TestPublicationConcurrentPreparedPRAdmitsOneCreate(t *testing.T) {
	q, target, _ := publicationFixture(t)
	sha := confirmedPublicationBranch(t, q, target)
	native := &concurrentPublicationPR{arrived: make(chan struct{}, 2), release: make(chan struct{})}
	p := Publisher{Queue: q, Git: publicationGitFake{sha: sha}, PRs: native}
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := p.OpenPR(t.Context(), target, "child title", "same body", true); results <- err }()
	}
	<-native.arrived
	<-native.arrived
	close(native.release)
	first, second := <-results, <-results
	if native.creates.Load() != 1 {
		t.Fatal("concurrent prepared intents repeated provider POST", native.creates.Load(), first, second)
	}
	if (first == nil) == (second == nil) {
		t.Fatal("exactly one prepared effect admission must win", first, second)
	}
	record, err := q.ChildPublication(t.Context(), target.Child.Identity, "pr")
	if err != nil || record.State != "confirmed" {
		t.Fatal(record, err)
	}
}
