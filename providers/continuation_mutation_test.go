package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/mutationreceipt"
)

type continuationLog struct {
	receipts       []mutationreceipt.Receipt
	failCompletion bool
}

func (l *continuationLog) RecordSemanticMutation(_ context.Context, r mutationreceipt.Receipt) error {
	if l.failCompletion && r.Phase == "completed" {
		l.failCompletion = false
		return errors.New("receipt fsync failed")
	}
	l.receipts = append(l.receipts, r)
	return nil
}

type continuationForge struct {
	mu               sync.Mutex
	comments         []restComment
	reviews          []githubNativeReview
	labels           []string
	state            string
	writes           map[string]int
	failAfterComment bool
	freshReads       int
}

func (f *continuationForge) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get(mutationreceipt.FreshReadHeader) == "true" {
		f.freshReads++
	}
	if r.Method != http.MethodGet {
		f.writes[path]++
	}
	reply := func(value any) {
		if err := json.NewEncoder(w).Encode(value); err != nil {
			panic(err)
		}
	}
	switch {
	case path == "/user":
		reply(map[string]string{"login": "bot"})
	case strings.HasSuffix(path, "/issues/7/comments"):
		if r.Method == http.MethodGet {
			reply(f.comments)
			return
		}
		var body struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			panic(err)
		}
		comment := restComment{ID: int64(len(f.comments) + 1), Body: body.Body, User: githubUser{Login: "bot"}, HTMLURL: "https://example.test/comment/1"}
		f.comments = append(f.comments, comment)
		if f.failAfterComment {
			f.failAfterComment = false
			w.WriteHeader(http.StatusInternalServerError)
			reply(map[string]string{"message": "response lost"})
			return
		}
		reply(comment)
	case strings.HasSuffix(path, "/issues/comments/1"):
		if len(f.comments) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPatch {
			var body struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				panic(err)
			}
			f.comments[0].Body = body.Body
		}
		reply(f.comments[0])
	case strings.HasSuffix(path, "/issues/7/labels"):
		if r.Method == http.MethodPost {
			var body struct {
				Labels json.RawMessage `json:"labels"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				panic(err)
			}
			var names []string
			if err := json.Unmarshal(body.Labels, &names); err != nil {
				var ids []int64
				if err := json.Unmarshal(body.Labels, &ids); err != nil {
					panic(err)
				}
				for range ids {
					names = append(names, "ready")
				}
			}
			f.labels = append(f.labels, names...)
		}
		labels := []githubLabel{}
		for _, name := range f.labels {
			labels = append(labels, githubLabel{Name: name})
		}
		reply(labels)
	case strings.Contains(path, "/issues/7/labels/"):
		f.labels = nil
		reply([]githubLabel{})
	case path == "/repos/acme/app/labels":
		reply([]map[string]any{{"id": 1, "name": "ready"}})
	case strings.HasSuffix(path, "/issues/7") || strings.HasSuffix(path, "/pulls/7"):
		if r.Method == http.MethodPatch {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				panic(err)
			}
			if body["state"] != "" {
				f.state = body["state"]
			}
		}
		labels := []githubLabel{}
		for _, name := range f.labels {
			labels = append(labels, githubLabel{Name: name})
		}
		reply(map[string]any{"number": 7, "state": f.state, "title": "item", "labels": labels, "html_url": "https://example.test/issues/7"})
	case strings.HasSuffix(path, "/pulls/7/reviews"):
		if r.Method == http.MethodPost {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				panic(err)
			}
			state := body["event"]
			switch state {
			case "APPROVE":
				state = "APPROVED"
			case "REQUEST_CHANGES":
				state = "CHANGES_REQUESTED"
			case "COMMENT":
				state = "COMMENTED"
			}
			review := githubNativeReview{ID: int64(len(f.reviews) + 1), Body: body["body"], CommitID: body["commit_id"], State: state, User: githubUser{Login: "bot"}, HTMLURL: "https://example.test/review/1"}
			f.reviews = append(f.reviews, review)
			reply(review)
			return
		}
		reply(f.reviews)
	default:
		w.WriteHeader(http.StatusNotFound)
		reply(map[string]string{"error": "unexpected " + r.Method + " " + path})
	}
}
func (f *continuationForge) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes[path]
}

type continuationProvider interface {
	CreateWorkItemComment(context.Context, RepositoryRef, string, string) (Comment, error)
	UpdateWorkItem(context.Context, UpdateWorkItemRequest) (WorkItem, error)
	UpdateComment(context.Context, RepositoryRef, string, string) error
	ClosePullRequest(context.Context, ClosePullRequestRequest) (ClosePullRequestResult, error)
	SubmitPullRequestReview(context.Context, PullRequestReviewRequest) (PullRequestReviewResult, error)
}

func continuationFixture(t *testing.T, kind ProviderKind) (*continuationForge, func(*mutationreceipt.Session) continuationProvider) {
	t.Helper()
	forge := &continuationForge{state: "open", writes: map[string]int{}, comments: []restComment{}, reviews: []githubNativeReview{}}
	server := httptest.NewServer(http.HandlerFunc(forge.serve))
	t.Cleanup(server.Close)
	return forge, func(session *mutationreceipt.Session) continuationProvider {
		if kind == ProviderGitHub {
			return NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL; p.maxRetries = 0; p.mutationSession = session })
		}
		return NewGiteaProvider(server.URL, "token", func(p *GiteaProvider) { p.maxRetries = 0; p.mutationSession = session })
	}
}
func resumedSession(log *continuationLog) *mutationreceipt.Session {
	return mutationreceipt.NewSession("continuation", log, log.receipts)
}

func TestContinuationRESTCompositeReconcilesPartialComment(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			forge, makeProvider := continuationFixture(t, kind)
			forge.failAfterComment = true
			log := &continuationLog{}
			provider := makeProvider(mutationreceipt.NewSession("source", log, nil))
			req := UpdateWorkItemRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, ID: "7", State: "closed", Comment: "published verdict", AddLabels: []string{"ready"}}
			if _, err := provider.UpdateWorkItem(t.Context(), req); err == nil {
				t.Fatal("expected partial provider action")
			}
			if _, err := makeProvider(resumedSession(log)).UpdateWorkItem(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/repos/acme/app/issues/7", "/repos/acme/app/issues/7/comments", "/repos/acme/app/issues/7/labels"} {
				if got := forge.count(path); got != 1 {
					t.Fatalf("%s mutations=%d, want one", path, got)
				}
			}
			if _, err := makeProvider(resumedSession(log)).UpdateWorkItem(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if got := forge.count("/repos/acme/app/issues/7/comments"); got != 1 {
				t.Fatalf("completed source comment repeated %d times", got)
			}
			req.Comment = "new verdict"
			if _, err := makeProvider(resumedSession(log)).UpdateWorkItem(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			forge.mu.Lock()
			freshReads := forge.freshReads
			forge.mu.Unlock()
			if freshReads == 0 {
				t.Fatal("reconciliation did not request fresh evidence")
			}
			if got := forge.count("/repos/acme/app/issues/7/comments"); got != 2 {
				t.Fatalf("new content suppressed: %d", got)
			}
		})
	}
}

func TestContinuationRESTCommentEvidenceAndReceiptFailure(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea} {
		for _, scenario := range []string{"completion failure", "missing", "foreign author", "changed body", "duplicate marker"} {
			t.Run(string(kind)+"/"+scenario, func(t *testing.T) {
				forge, makeProvider := continuationFixture(t, kind)
				log := &continuationLog{failCompletion: scenario == "completion failure"}
				repo := RepositoryRef{Owner: "acme", Name: "app"}
				_, err := makeProvider(mutationreceipt.NewSession("source", log, nil)).CreateWorkItemComment(t.Context(), repo, "7", "body")
				if (err != nil) != (scenario == "completion failure") {
					t.Fatalf("source error=%v", err)
				}
				forge.mu.Lock()
				switch scenario {
				case "missing":
					forge.comments = nil
				case "foreign author":
					forge.comments[0].User.Login = "other"
				case "changed body":
					forge.comments[0].Body = strings.Replace(forge.comments[0].Body, "body", "edited", 1)
				case "duplicate marker":
					copy := forge.comments[0]
					copy.ID++
					forge.comments = append(forge.comments, copy)
				}
				forge.mu.Unlock()
				comment, err := makeProvider(resumedSession(log)).CreateWorkItemComment(t.Context(), repo, "7", "body")
				if scenario == "completion failure" {
					if err != nil || comment.ID != "1" {
						t.Fatalf("recover durable provider receipt: %+v %v", comment, err)
					}
				} else if !errors.Is(err, ErrMutationUnresolved) {
					t.Fatalf("unsafe evidence accepted: %v", err)
				}
				if forge.count("/repos/acme/app/issues/7/comments") != 1 {
					t.Fatal("ambiguous evidence duplicated a public comment")
				}
			})
		}
	}
}

func TestContinuationRESTReviewAndCloseEvidence(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			forge, makeProvider := continuationFixture(t, kind)
			log := &continuationLog{}
			repo := RepositoryRef{Owner: "acme", Name: "app"}
			source := makeProvider(mutationreceipt.NewSession("source", log, nil))
			req := PullRequestReviewRequest{Repository: repo, PullID: "7", CommitSHA: "head", Body: "verdict", Decision: ReviewDecisionApproved}
			if _, err := source.SubmitPullRequestReview(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			closeReq := ClosePullRequestRequest{Repository: repo, PullID: "7", Comment: "closeout"}
			if _, err := source.ClosePullRequest(t.Context(), closeReq); err != nil {
				t.Fatal(err)
			}
			resumed := makeProvider(resumedSession(log))
			if review, err := resumed.SubmitPullRequestReview(t.Context(), req); err != nil || review.ID != 1 {
				t.Fatalf("review=%+v err=%v", review, err)
			}
			if _, err := resumed.ClosePullRequest(t.Context(), closeReq); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/repos/acme/app/pulls/7", "/repos/acme/app/pulls/7/reviews", "/repos/acme/app/issues/7/comments"} {
				if got := forge.count(path); got != 1 {
					t.Fatalf("%s repeated: %d", path, got)
				}
			}
			req.CommitSHA = "new-head"
			if _, err := resumed.SubmitPullRequestReview(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if forge.count("/repos/acme/app/pulls/7/reviews") != 2 {
				t.Fatal("new review commit suppressed")
			}
			forge.mu.Lock()
			forge.state = "open"
			forge.mu.Unlock()
			if _, err := makeProvider(resumedSession(log)).ClosePullRequest(t.Context(), closeReq); !errors.Is(err, ErrMutationUnresolved) {
				t.Fatalf("reopened PR evidence accepted: %v", err)
			}
		})
	}
}

func TestContinuationRESTInertByDefault(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			forge, makeProvider := continuationFixture(t, kind)
			provider := makeProvider(nil)
			for range 2 {
				if _, err := provider.CreateWorkItemComment(t.Context(), RepositoryRef{Owner: "acme", Name: "app"}, "7", "body"); err != nil {
					t.Fatal(err)
				}
			}
			forge.mu.Lock()
			defer forge.mu.Unlock()
			if len(forge.comments) != 2 || strings.Contains(forge.comments[0].Body, "goobers:mutation") {
				t.Fatal("dormant slice changed production behavior")
			}
		})
	}
}

func TestContinuationRESTKeyedUpdateDoesNotAdoptLegacyMarker(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			forge, makeProvider := continuationFixture(t, kind)
			forge.comments = append(forge.comments, restComment{ID: 99, Body: "old body\n\n" + OperationCommentMarker("reused"), User: githubUser{Login: "bot"}})
			log := &continuationLog{}
			req := UpdateWorkItemRequest{Repository: RepositoryRef{Owner: "acme", Name: "app"}, ID: "7", State: "closed", Comment: "current body", IdempotencyKey: "reused"}
			if _, err := makeProvider(mutationreceipt.NewSession("source", log, nil)).UpdateWorkItem(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if _, err := makeProvider(resumedSession(log)).UpdateWorkItem(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if forge.count("/repos/acme/app/issues/7/comments") != 1 || forge.count("/repos/acme/app/issues/7") != 1 {
				t.Fatal("legacy marker bypassed semantic action or replay duplicated it")
			}
		})
	}
}

func TestContinuationRESTCommentEditAndLabelRemoval(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderGitea} {
		t.Run(string(kind), func(t *testing.T) {
			forge, makeProvider := continuationFixture(t, kind)
			log := &continuationLog{}
			repo := RepositoryRef{Owner: "acme", Name: "app"}
			source := makeProvider(mutationreceipt.NewSession("source", log, nil))
			if _, err := source.CreateWorkItemComment(t.Context(), repo, "7", "original"); err != nil {
				t.Fatal(err)
			}
			if err := source.UpdateComment(t.Context(), repo, "1", "edited"); err != nil {
				t.Fatal(err)
			}
			forge.labels = []string{"ready"}
			req := UpdateWorkItemRequest{Repository: repo, ID: "7", RemoveLabels: []string{"ready"}}
			if _, err := source.UpdateWorkItem(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			resumed := makeProvider(resumedSession(log))
			if err := resumed.UpdateComment(t.Context(), repo, "1", "edited"); err != nil {
				t.Fatal(err)
			}
			if _, err := resumed.UpdateWorkItem(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if forge.count("/repos/acme/app/issues/comments/1") != 1 {
				t.Fatal("edit repeated")
			}
			forge.mu.Lock()
			forge.comments[0].Body = "human edit"
			forge.mu.Unlock()
			if err := resumed.UpdateComment(t.Context(), repo, "1", "edited"); !errors.Is(err, ErrMutationUnresolved) {
				t.Fatalf("human edit overwritten: %v", err)
			}
		})
	}
}
