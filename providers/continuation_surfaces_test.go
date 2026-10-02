package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/mutationreceipt"
)

type surfaceProvider interface {
	continuationProvider
	OpenPullRequest(context.Context, PullRequestRequest) (PullRequestResult, error)
	RequestReview(context.Context, ReviewRequest) error
	DeleteComment(context.Context, RepositoryRef, string) error
	EnsureWorkItemLabels(context.Context, RepositoryRef, []WorkItemLabel) (EnsureWorkItemLabelsResult, error)
}

type surfaceForge struct {
	*continuationForge
	pull        continuationPull
	labels      []giteaLabel
	statuses    []continuationStatus
	replies     []githubInlineReviewComment
	resolved    bool
	threadRepo  string
	failResolve bool
}

func surfaceFixture(t *testing.T, kind ProviderKind) (*surfaceForge, func(*mutationreceipt.Session) surfaceProvider) {
	t.Helper()
	forge := &surfaceForge{continuationForge: &continuationForge{state: "open", writes: map[string]int{}}, threadRepo: "acme/app", labels: []giteaLabel{}, statuses: []continuationStatus{}, replies: []githubInlineReviewComment{}}
	forge.pull.Number = 7
	forge.pull.State = "open"
	forge.pull.Head.SHA = "head"
	forge.pull.Head.Ref = "work"
	forge.pull.Base.Ref = "main"
	forge.pull.Head.Repo = &restRepository{Name: "app", Owner: githubUser{Login: "acme"}}
	forge.pull.User.Login = "bot"
	server := httptest.NewServer(http.HandlerFunc(forge.serveSurface))
	t.Cleanup(server.Close)
	return forge, func(session *mutationreceipt.Session) surfaceProvider {
		if kind == ProviderGitHub {
			return NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL; p.mutationSession = session })
		}
		return NewGiteaProvider(server.URL, "token", func(p *GiteaProvider) { p.mutationSession = session })
	}
}

func (f *surfaceForge) serveSurface(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	write := func(v any) {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			panic(err)
		}
	}
	decode := func(v any) {
		if err := json.NewDecoder(r.Body).Decode(v); err != nil {
			panic(err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet && path != "/graphql" {
		f.writes[path]++
	}
	switch path {
	case "/repos/acme/app/pulls/7":
		if r.Method == http.MethodPatch {
			var body struct{ Title, Body string }
			decode(&body)
			f.pull.Title = body.Title
			f.pull.Body = body.Body
		}
		write(f.pull)
	case "/repos/acme/app/pulls":
		if r.Method == http.MethodPost {
			var body struct {
				Title, Body, Head, Base string
				Draft                   bool
			}
			decode(&body)
			f.pull.Title = body.Title
			f.pull.Body = body.Body
			f.pull.Draft = body.Draft
			f.pull.Number = 7
			f.pull.State = "open"
			write(f.pull)
		} else if f.pull.Number == 0 {
			write([]continuationPull{})
		} else {
			write([]continuationPull{f.pull})
		}
	case "/repos/acme/app/pulls/7/requested_reviewers":
		var body struct{ Reviewers []string }
		decode(&body)
		for _, login := range body.Reviewers {
			f.pull.RequestedReviewers = append(f.pull.RequestedReviewers, githubUser{Login: login})
		}
		write(f.pull)
	case "/repos/acme/app/labels":
		if r.Method == http.MethodPost {
			var label giteaLabel
			decode(&label)
			label.ID = int64(len(f.labels) + 1)
			f.labels = append(f.labels, label)
			write(label)
		} else {
			write(f.labels)
		}
	case "/repos/acme/app/statuses/head":
		if r.Method == http.MethodPost {
			var body map[string]string
			decode(&body)
			status := continuationStatus{ID: len(f.statuses) + 1, Context: body["context"], State: body["state"], Description: body["description"], TargetURL: body["target_url"], Creator: githubUser{Login: "bot"}}
			f.statuses = append(f.statuses, status)
			write(status)
		} else {
			write(f.statuses)
		}
	case "/repos/acme/app/issues/comments/1":
		if r.Method == http.MethodDelete {
			f.comments = nil
		}
		if len(f.comments) == 0 {
			w.WriteHeader(http.StatusNotFound)
		} else {
			write(f.comments[0])
		}
	case "/repos/acme/app/pulls/7/comments":
		write(f.replies)
	case "/repos/acme/app/pulls/7/comments/41/replies":
		var body map[string]string
		decode(&body)
		reply := githubInlineReviewComment{ID: int64(50 + len(f.replies)), Body: body["body"], InReplyTo: 41, User: githubUser{Login: "bot"}}
		f.replies = append(f.replies, reply)
		write(reply)
	case "/graphql":
		var body struct{ Query string }
		decode(&body)
		if isGraphQLMutation(body.Query) {
			f.writes["resolve"]++
			f.resolved = true
			if f.failResolve {
				f.failResolve = false
				w.WriteHeader(http.StatusInternalServerError)
			}
			write(map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{"thread": map[string]any{"id": "thread-1", "isResolved": true}}}})
		} else {
			write(map[string]any{"data": map[string]any{"node": map[string]any{"id": "thread-1", "isResolved": f.resolved, "repository": map[string]any{"nameWithOwner": f.threadRepo}, "pullRequest": map[string]any{"number": 7, "headRefOid": f.pull.Head.SHA}}}})
		}
	default:
		if r.Method != http.MethodGet {
			f.writes[path]--
		}
		f.mu.Unlock()
		f.serve(w, r)
		return
	}
	f.mu.Unlock()
}

func TestContinuationSurfaceCrashReconciliation(t *testing.T) {
	repo := RepositoryRef{Owner: "acme", Name: "app"}
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea} {
		for _, action := range []string{"delete", "request", "ensure", "open", "reuse"} {
			t.Run(string(kind)+"/"+action, func(t *testing.T) {
				forge, makeProvider := surfaceFixture(t, kind)
				log := &continuationLog{}
				source := makeProvider(mutationreceipt.NewSession("source", log, nil))
				path := "/repos/acme/app/issues/comments/1"
				invoke := func(p surfaceProvider) error { return p.DeleteComment(t.Context(), repo, "1") }
				switch action {
				case "delete":
					forge.comments = []restComment{{ID: 1, Body: "old"}}
				case "request":
					path = "/repos/acme/app/pulls/7/requested_reviewers"
					invoke = func(p surfaceProvider) error {
						return p.RequestReview(t.Context(), ReviewRequest{Repository: repo, PullID: "7", Reviewers: []string{"alice"}})
					}
				case "ensure":
					path = "/repos/acme/app/labels"
					invoke = func(p surfaceProvider) error {
						_, err := p.EnsureWorkItemLabels(t.Context(), repo, []WorkItemLabel{{Name: "ready", Color: "abcdef", Description: "ready"}})
						return err
					}
				case "open", "reuse":
					path = "/repos/acme/app/pulls/7"
					if action == "open" {
						forge.pull.Number = 0
						path = "/repos/acme/app/pulls"
					}
					invoke = func(p surfaceProvider) error {
						_, err := p.OpenPullRequest(t.Context(), PullRequestRequest{Repository: repo, Head: "work", Base: "main", Title: "change", Body: "semantic body", RunID: "source"})
						return err
					}
				}
				injectCommittedResponseLoss(t, source, path, "500")
				if err := invoke(source); err == nil {
					t.Fatal("lost response reported success")
				}
				if forge.count(path) != 1 || log.receipts[len(log.receipts)-1].Phase != "intent" {
					t.Fatal("lost response repeated or completed")
				}
				if err := invoke(makeProvider(resumedSession(log))); err != nil {
					t.Fatal(err)
				}
				if err := invoke(makeProvider(resumedSession(log))); err != nil {
					t.Fatal(err)
				}
				if forge.count(path) != 1 || log.receipts[len(log.receipts)-1].Phase != "completed" {
					t.Fatalf("reconciliation did not adopt: writes=%d", forge.count(path))
				}
			})
		}
	}
}

func TestContinuationStatusUsesLatestContextAndActor(t *testing.T) {
	for _, change := range []string{"none", "later verdict", "foreign actor", "description"} {
		t.Run(change, func(t *testing.T) {
			forge, makeProvider := surfaceFixture(t, ProviderGitea)
			log := &continuationLog{}
			req := PullRequestStatusRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "7", HeadSHA: "head", Name: "verdict", State: CheckStatePassing, Description: "checked", TargetURL: "https://example.test/result"}
			source := makeProvider(mutationreceipt.NewSession("source", log, nil))
			path := "/repos/acme/app/statuses/head"
			injectCommittedResponseLoss(t, source, path, "transport")
			if _, err := source.(*GiteaProvider).PublishPullRequestStatus(t.Context(), req); err == nil {
				t.Fatal("lost response accepted")
			}
			forge.mu.Lock()
			switch change {
			case "later verdict":
				newer := forge.statuses[0]
				newer.ID = 2
				newer.State = "failure"
				forge.statuses = append(forge.statuses, newer)
			case "foreign actor":
				forge.statuses[0].Creator.Login = "other"
			case "description":
				forge.statuses[0].Description = "different"
			}
			forge.mu.Unlock()
			_, err := makeProvider(resumedSession(log)).(*GiteaProvider).PublishPullRequestStatus(t.Context(), req)
			if change == "none" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrMutationUnresolved) {
				t.Fatalf("changed evidence=%v", err)
			}
			if forge.count(path) != 1 {
				t.Fatal("status repeated")
			}
		})
	}
}

func TestContinuationThreadReplyAndResolveAreIndependent(t *testing.T) {
	forge, makeProvider := surfaceFixture(t, ProviderGitHub)
	log := &continuationLog{}
	repo := RepositoryRef{Owner: "acme", Name: "app"}
	req := PullRequestReviewThreadReply{Repository: repo, PullID: "7", CommentID: 41, Body: "addressed"}
	source := makeProvider(mutationreceipt.NewSession("source", log, nil)).(*GitHubProvider)
	if _, err := source.ReplyPullRequestReviewThread(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	forge.failResolve = true
	if err := source.ResolvePullRequestReviewThread(t.Context(), repo, "thread-1"); err == nil {
		t.Fatal("lost resolve response accepted")
	}
	resumed := makeProvider(resumedSession(log)).(*GitHubProvider)
	if _, err := resumed.ReplyPullRequestReviewThread(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := resumed.ResolvePullRequestReviewThread(t.Context(), repo, "thread-1"); err != nil {
		t.Fatal(err)
	}
	if forge.count("resolve") != 1 || forge.count("/repos/acme/app/pulls/7/comments/41/replies") != 1 {
		t.Fatal("thread mutation repeated")
	}
	forge.mu.Lock()
	forge.resolved = false
	forge.mu.Unlock()
	if err := resumed.ResolvePullRequestReviewThread(t.Context(), repo, "thread-1"); !errors.Is(err, ErrMutationUnresolved) {
		t.Fatalf("human unresolve overwritten: %v", err)
	}
	forge.mu.Lock()
	forge.threadRepo = "other/repo"
	forge.mu.Unlock()
	if err := resumed.ResolvePullRequestReviewThread(t.Context(), repo, "thread-1"); !errors.Is(err, ErrMutationUnresolved) {
		t.Fatalf("foreign repository accepted: %v", err)
	}
}

func TestContinuationSurfaceChangedPayloadAndState(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			forge, makeProvider := surfaceFixture(t, kind)
			log := &continuationLog{}
			repo := RepositoryRef{Owner: "acme", Name: "app"}
			source := makeProvider(mutationreceipt.NewSession("source", log, nil))
			req := PullRequestRequest{Repository: repo, Head: "work", Base: "main", Title: "title", Body: "body", RunID: "source"}
			if _, err := source.OpenPullRequest(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			req.RunID = "continuation"
			resumed := makeProvider(resumedSession(log))
			if _, err := resumed.OpenPullRequest(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if forge.count("/repos/acme/app/pulls/7") != 1 {
				t.Fatal("run footer changed semantic identity")
			}
			req.Body = "new body"
			if _, err := resumed.OpenPullRequest(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if forge.count("/repos/acme/app/pulls/7") != 2 {
				t.Fatal("changed semantic body suppressed")
			}
			forge.mu.Lock()
			forge.pull.Title = "human title"
			forge.mu.Unlock()
			if _, err := resumed.OpenPullRequest(t.Context(), req); !errors.Is(err, ErrMutationUnresolved) {
				t.Fatalf("human edit ignored: %v", err)
			}
			review := ReviewRequest{Repository: repo, PullID: "7", Reviewers: []string{"alice", "bob"}}
			if err := resumed.RequestReview(t.Context(), review); err != nil {
				t.Fatal(err)
			}
			forge.mu.Lock()
			forge.pull.RequestedReviewers = nil
			forge.mu.Unlock()
			if err := resumed.RequestReview(t.Context(), review); !errors.Is(err, ErrMutationUnresolved) {
				t.Fatalf("removed requests repeated: %v", err)
			}
		})
	}
}

func TestContinuationGiteaLabelDefinitionCrashBeforeApplication(t *testing.T) {
	forge, makeProvider := surfaceFixture(t, ProviderGitea)
	log := &continuationLog{}
	source := makeProvider(mutationreceipt.NewSession("source", log, nil))
	path := "/repos/acme/app/labels"
	injectCommittedResponseLoss(t, source, path, "transport")
	req := UpdateWorkItemRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, ID: "7", AddLabels: []string{"ready"}}
	if _, err := source.UpdateWorkItem(t.Context(), req); err == nil {
		t.Fatal("lost label definition response accepted")
	}
	if forge.count(path) != 1 || forge.count("/repos/acme/app/issues/7/labels") != 0 {
		t.Fatal("definition/application boundary lost")
	}
	if _, err := makeProvider(resumedSession(log)).UpdateWorkItem(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if forge.count(path) != 1 || forge.count("/repos/acme/app/issues/7/labels") != 1 {
		t.Fatal("partial label action repeated")
	}
}

func TestContinuationThreadReplyRequiresPhysicalParentAndAuthor(t *testing.T) {
	for _, change := range []string{"none", "parent", "author", "body", "missing", "duplicate"} {
		t.Run(change, func(t *testing.T) {
			forge, makeProvider := surfaceFixture(t, ProviderGitHub)
			log := &continuationLog{}
			source := makeProvider(mutationreceipt.NewSession("source", log, nil)).(*GitHubProvider)
			path := "/repos/acme/app/pulls/7/comments/41/replies"
			injectCommittedResponseLoss(t, source, path, "500")
			req := PullRequestReviewThreadReply{Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "7", CommentID: 41, Body: "addressed"}
			if _, err := source.ReplyPullRequestReviewThread(t.Context(), req); err == nil {
				t.Fatal("lost reply response accepted")
			}
			forge.mu.Lock()
			switch change {
			case "parent":
				forge.replies[0].InReplyTo = 42
			case "author":
				forge.replies[0].User.Login = "someone-else"
			case "body":
				forge.replies[0].Body = "changed\n\n" + continuationMarker(log.receipts[0])
			case "missing":
				forge.replies = nil
			case "duplicate":
				forge.replies = append(forge.replies, forge.replies[0])
			}
			forge.mu.Unlock()
			_, err := makeProvider(resumedSession(log)).(*GitHubProvider).ReplyPullRequestReviewThread(t.Context(), req)
			if change == "none" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrMutationUnresolved) {
				t.Fatalf("ambiguous reply evidence accepted: %v", err)
			}
			if forge.count(path) != 1 {
				t.Fatal("reply repeated")
			}
		})
	}
}

func TestContinuationPullMissingEvidenceDoesNotCreateOrPatch(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			forge, makeProvider := surfaceFixture(t, kind)
			forge.pull.Number = 0
			log := &continuationLog{}
			source := makeProvider(mutationreceipt.NewSession("source", log, nil))
			path := "/repos/acme/app/pulls"
			injectCommittedResponseLoss(t, source, path, "transport")
			req := PullRequestRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, Head: "work", Base: "main", Title: "title", Body: "body"}
			if _, err := source.OpenPullRequest(t.Context(), req); err == nil {
				t.Fatal("lost create response accepted")
			}
			forge.mu.Lock()
			forge.pull.Body = "human body without marker"
			forge.mu.Unlock()
			if _, err := makeProvider(resumedSession(log)).OpenPullRequest(t.Context(), req); !errors.Is(err, ErrMutationUnresolved) {
				t.Fatalf("missing marker accepted: %v", err)
			}
			if forge.count(path) != 1 || forge.count(path+"/7") != 0 {
				t.Fatal("uncertain create switched to patch")
			}
		})
	}
}

func TestContinuationReviewerBatchCrashPreservesIndividualRequests(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			forge, makeProvider := surfaceFixture(t, kind)
			log := &continuationLog{}
			source := makeProvider(mutationreceipt.NewSession("source", log, nil))
			path := "/repos/acme/app/pulls/7/requested_reviewers"
			injectCommittedResponseLoss(t, source, path, "transport")
			req := ReviewRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, PullID: "7", Reviewers: []string{"alice", "bob"}}
			if err := source.RequestReview(t.Context(), req); err == nil {
				t.Fatal("lost first reviewer response accepted")
			}
			if err := makeProvider(resumedSession(log)).RequestReview(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if forge.count(path) != 2 {
				t.Fatalf("reviewer requests=%d, want one per reviewer", forge.count(path))
			}
			forge.mu.Lock()
			defer forge.mu.Unlock()
			if len(forge.pull.RequestedReviewers) != 2 || forge.pull.RequestedReviewers[0].Login != "alice" || forge.pull.RequestedReviewers[1].Login != "bob" {
				t.Fatal("partial batch repeated or suppressed a reviewer")
			}
		})
	}
}
