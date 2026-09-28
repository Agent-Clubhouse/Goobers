package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The attribution contract a caller of a write relies on: under
// SetAttribution the provider stores the body with the attribution footer and
// returns that stored body. Code that reads back text Goobers wrote must
// therefore compare StripAttribution(stored) with strings.TrimSpace(intended),
// never the raw body. These tests pin the contract on every provider that
// stamps writes, with a cost receipt in the attribution, against fakes that
// store bodies verbatim as the forges do.

// contractIntended carries surrounding whitespace so the contract's
// TrimSpace half is exercised too.
const contractIntended = "\n  Contract body.\n\nSecond paragraph.  \n"

func contractAttribution() Attribution {
	nanoAIU := int64(2_500_000_000)
	return Attribution{
		Instance: "example-instance", Gaggle: "example-gaggle", Workflow: "example-workflow",
		Task: "contract-task", Goober: "deterministic", Run: "run-attribution-contract",
		Cost: &CostReceipt{JournalSequence: 3, Model: "example-model", NanoAIU: &nanoAIU},
	}
}

// contractStore keeps written bodies exactly as a forge stores them.
type contractStore struct {
	mu     sync.Mutex
	bodies map[string]string
}

func (s *contractStore) put(key, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bodies == nil {
		s.bodies = map[string]string{}
	}
	s.bodies[key] = body
}

func (s *contractStore) get(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[key]
}

// attributedWrite is one provider write under the contract. write performs it
// and returns the body the provider handed back: the returned body for a
// create or reply, the listed body for an update, which returns none.
type attributedWrite struct {
	name   string
	action string
	key    string
	write  func(t *testing.T) string
}

func assertAttributedWriteContract(t *testing.T, store *contractStore, write attributedWrite) {
	t.Helper()
	returned := write.write(t)
	if stored := store.get(write.key); returned != stored {
		t.Errorf("returned body = %q, want the stored body %q", returned, stored)
	}
	if got, want := StripAttribution(returned), strings.TrimSpace(contractIntended); got != want {
		t.Errorf("StripAttribution(returned) = %q, want %q", got, want)
	}
	parsed, ok, err := ParseAttribution(returned)
	if err != nil || !ok {
		t.Fatalf("returned body carries no valid attribution: ok=%v err=%v body=%q", ok, err, returned)
	}
	if parsed.Action != write.action || parsed.Task != "contract-task" || parsed.Run != "run-attribution-contract" {
		t.Errorf("attribution = %+v, want action %q for the contract run", parsed, write.action)
	}
	if parsed.Cost == nil || parsed.Cost.NanoAIU == nil || *parsed.Cost.NanoAIU != 2_500_000_000 {
		t.Errorf("attribution cost = %+v, want the cost receipt", parsed.Cost)
	}
	restamped, err := StampAttribution(returned, contractAttribution(), write.action)
	if err != nil {
		t.Fatalf("re-stamp returned body: %v", err)
	}
	if markers := len(attributionMarkerStartPattern.FindAllStringIndex(restamped, -1)); markers != 1 {
		t.Errorf("re-stamped body has %d attribution markers, want exactly one: %q", markers, restamped)
	}
	if got := StripAttribution(restamped); got != strings.TrimSpace(contractIntended) {
		t.Errorf("StripAttribution(re-stamped) = %q, want %q", got, strings.TrimSpace(contractIntended))
	}
}

func decodeContractJSON(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode %s %s: %v", r.Method, r.URL.Path, err)
	}
	return body
}

func contractString(value any) string {
	text, _ := value.(string)
	return text
}

func TestAttributedWritesReturnStoredBodyOnGitHub(t *testing.T) {
	store := &contractStore{}
	const prefix = "/repos/example-org/example-repo"
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/issues", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		body := decodeContractJSON(t, r)
		store.put("issue", contractString(body["body"]))
		writeJSON(t, w, map[string]any{"number": 1, "title": body["title"], "body": store.get("issue"), "state": "open"})
	})
	mux.HandleFunc(prefix+"/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, []map[string]any{{"id": 11, "body": store.get("comment"), "user": map[string]string{"login": "goobers"}}})
			return
		}
		assertMethod(t, r, http.MethodPost)
		store.put("comment", contractString(decodeContractJSON(t, r)["body"]))
		writeJSON(t, w, map[string]any{"id": 11, "body": store.get("comment"), "user": map[string]string{"login": "goobers"}})
	})
	mux.HandleFunc(prefix+"/issues/comments/11", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		store.put("comment", contractString(decodeContractJSON(t, r)["body"]))
		writeJSON(t, w, map[string]any{"id": 11, "body": store.get("comment")})
	})
	mux.HandleFunc(prefix+"/pulls/2/comments/5/replies", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		store.put("reply", contractString(decodeContractJSON(t, r)["body"]))
		writeJSON(t, w, map[string]any{"id": 6, "body": store.get("reply"), "in_reply_to_id": 5})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGitHubProvider("token", func(p *GitHubProvider) { p.BaseURL = server.URL })
	provider.SetAttribution(contractAttribution())
	repo := RepositoryRef{Owner: "example-org", Name: "example-repo"}
	ctx := context.Background()
	for _, write := range []attributedWrite{
		{name: "CreateWorkItem", action: "issue-create", key: "issue", write: func(t *testing.T) string {
			item, err := provider.CreateWorkItem(ctx, CreateWorkItemRequest{Repository: repo, Title: "Contract", Body: contractIntended})
			if err != nil {
				t.Fatalf("CreateWorkItem: %v", err)
			}
			return item.Body
		}},
		{name: "CreateWorkItemComment", action: "comment", key: "comment", write: func(t *testing.T) string {
			comment, err := provider.CreateWorkItemComment(ctx, repo, "1", contractIntended)
			if err != nil {
				t.Fatalf("CreateWorkItemComment: %v", err)
			}
			return comment.Body
		}},
		{name: "UpdateComment", action: "comment-update", key: "comment", write: func(t *testing.T) string {
			if err := provider.UpdateComment(ctx, repo, "11", contractIntended); err != nil {
				t.Fatalf("UpdateComment: %v", err)
			}
			return onlyListedComment(t, provider, repo, "1")
		}},
		{name: "ReplyPullRequestReviewThread", action: "review-thread-reply", key: "reply", write: func(t *testing.T) string {
			reply, err := provider.ReplyPullRequestReviewThread(ctx, PullRequestReviewThreadReply{
				Repository: repo, PullID: "2", ThreadID: "PRRT_contract", CommentID: 5, Body: contractIntended,
			})
			if err != nil {
				t.Fatalf("ReplyPullRequestReviewThread: %v", err)
			}
			return reply.Body
		}},
	} {
		t.Run(write.name, func(t *testing.T) { assertAttributedWriteContract(t, store, write) })
	}
}

func TestAttributedWritesReturnStoredBodyOnGitea(t *testing.T) {
	store := &contractStore{}
	const prefix = "/api/v1/repos/example-org/example-repo"
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/issues", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		body := decodeContractJSON(t, r)
		store.put("issue", contractString(body["body"]))
		writeJSON(t, w, map[string]any{"id": 1, "number": 1, "title": body["title"], "body": store.get("issue"), "state": "open"})
	})
	mux.HandleFunc(prefix+"/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, []map[string]any{{"id": 11, "body": store.get("comment"), "user": map[string]string{"login": "goobers"}}})
			return
		}
		assertMethod(t, r, http.MethodPost)
		store.put("comment", contractString(decodeContractJSON(t, r)["body"]))
		writeJSON(t, w, map[string]any{"id": 11, "body": store.get("comment"), "user": map[string]string{"login": "goobers"}})
	})
	mux.HandleFunc(prefix+"/issues/comments/11", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		store.put("comment", contractString(decodeContractJSON(t, r)["body"]))
		writeJSON(t, w, map[string]any{"id": 11, "body": store.get("comment")})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewGiteaProvider(server.URL, "token")
	provider.SetAttribution(contractAttribution())
	repo := RepositoryRef{Owner: "example-org", Name: "example-repo"}
	ctx := context.Background()
	for _, write := range []attributedWrite{
		{name: "CreateWorkItem", action: "issue-create", key: "issue", write: func(t *testing.T) string {
			item, err := provider.CreateWorkItem(ctx, CreateWorkItemRequest{Repository: repo, Title: "Contract", Body: contractIntended})
			if err != nil {
				t.Fatalf("CreateWorkItem: %v", err)
			}
			return item.Body
		}},
		{name: "CreateWorkItemComment", action: "comment", key: "comment", write: func(t *testing.T) string {
			comment, err := provider.CreateWorkItemComment(ctx, repo, "1", contractIntended)
			if err != nil {
				t.Fatalf("CreateWorkItemComment: %v", err)
			}
			return comment.Body
		}},
		{name: "UpdateComment", action: "comment-update", key: "comment", write: func(t *testing.T) string {
			if err := provider.UpdateComment(ctx, repo, "11", contractIntended); err != nil {
				t.Fatalf("UpdateComment: %v", err)
			}
			return onlyListedComment(t, provider, repo, "1")
		}},
	} {
		t.Run(write.name, func(t *testing.T) { assertAttributedWriteContract(t, store, write) })
	}
}

func TestAttributedWritesReturnStoredBodyOnADO(t *testing.T) {
	store := &contractStore{}
	const pr = "/org/project/_apis/git/repositories/repo/pullrequests/42"
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/workitems/$Issue", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		var patch []adoPatchOperation
		decodeJSON(t, r, &patch)
		fields := map[string]any{"System.WorkItemType": "Issue", "System.State": "New"}
		for _, op := range patch {
			if name, ok := strings.CutPrefix(op.Path, "/fields/"); ok {
				fields[name] = op.Value
			}
		}
		store.put("issue", contractString(fields["System.Description"]))
		writeJSON(t, w, map[string]any{"id": 51, "rev": 1, "url": "item-url", "fields": fields})
	})
	mux.HandleFunc("/org/project/_apis/wit/workItems/7/comments", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		store.put("comment", contractString(decodeContractJSON(t, r)["text"]))
		writeJSON(t, w, map[string]any{"commentId": 9, "text": store.get("comment")})
	})
	mux.HandleFunc(pr+"/threads", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, map[string]any{"value": []map[string]any{{
				"id": 7, "comments": []map[string]any{adoContractThreadComment(1, 0, store.get("thread"))},
			}}})
			return
		}
		assertMethod(t, r, http.MethodPost)
		comments, _ := decodeContractJSON(t, r)["comments"].([]any)
		if len(comments) != 1 {
			t.Fatalf("thread create comments = %v, want one", comments)
		}
		first, _ := comments[0].(map[string]any)
		store.put("thread", contractString(first["content"]))
		writeJSON(t, w, map[string]any{"id": 7, "comments": []map[string]any{adoContractThreadComment(1, 0, store.get("thread"))}})
	})
	mux.HandleFunc(pr+"/threads/7/comments/1", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPatch)
		store.put("thread", contractString(decodeContractJSON(t, r)["content"]))
		writeJSON(t, w, adoContractThreadComment(1, 0, store.get("thread")))
	})
	mux.HandleFunc(pr+"/threads/5/comments", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		store.put("reply", contractString(decodeContractJSON(t, r)["content"]))
		writeJSON(t, w, adoContractThreadComment(3, 1, store.get("reply")))
	})
	server := httptest.NewServer(withADOTestWorkItemsBatch(t, mux))
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	provider.SetAttribution(contractAttribution())
	repo := RepositoryRef{Name: "repo", Project: "project"}
	ctx := context.Background()
	for _, write := range []attributedWrite{
		{name: "CreateWorkItem", action: "issue-create", key: "issue", write: func(t *testing.T) string {
			item, err := provider.CreateWorkItem(ctx, CreateWorkItemRequest{Repository: repo, Title: "Contract", Type: "Issue", Body: contractIntended})
			if err != nil {
				t.Fatalf("CreateWorkItem: %v", err)
			}
			return item.Body
		}},
		{name: "CreateWorkItemComment", action: "comment", key: "comment", write: func(t *testing.T) string {
			comment, err := provider.CreateWorkItemComment(ctx, repo, "7", contractIntended)
			if err != nil {
				t.Fatalf("CreateWorkItemComment: %v", err)
			}
			return comment.Body
		}},
		{name: "PostPullRequestThreadComment", action: "pull-request-comment", key: "thread", write: func(t *testing.T) string {
			comment, err := provider.PostPullRequestThreadComment(ctx, repo, "42", contractIntended)
			if err != nil {
				t.Fatalf("PostPullRequestThreadComment: %v", err)
			}
			return comment.Body
		}},
		{name: "UpdatePullRequestThreadComment", action: "pull-request-comment-update", key: "thread", write: func(t *testing.T) string {
			if err := provider.UpdatePullRequestThreadComment(ctx, repo, "42/7/1", contractIntended); err != nil {
				t.Fatalf("UpdatePullRequestThreadComment: %v", err)
			}
			comments, err := provider.ListPullRequestThreadComments(ctx, repo, "42")
			if err != nil || len(comments) != 1 {
				t.Fatalf("ListPullRequestThreadComments = %+v, %v; want one", comments, err)
			}
			return comments[0].Body
		}},
		{name: "ReplyPullRequestReviewThread", action: "review-thread-reply", key: "reply", write: func(t *testing.T) string {
			reply, err := provider.ReplyPullRequestReviewThread(ctx, PullRequestReviewThreadReply{
				Repository: repo, PullID: "42", ThreadID: "42/5", CommentID: 1, Body: contractIntended,
			})
			if err != nil {
				t.Fatalf("ReplyPullRequestReviewThread: %v", err)
			}
			return reply.Body
		}},
	} {
		t.Run(write.name, func(t *testing.T) { assertAttributedWriteContract(t, store, write) })
	}
}

func adoContractThreadComment(id, parent int, content string) map[string]any {
	return map[string]any{
		"id": id, "parentCommentId": parent, "content": content, "commentType": "text",
		"author": map[string]string{"id": "self-guid", "displayName": "Goobers"},
	}
}

// onlyListedComment lists issue id's comments and returns the only one.
func onlyListedComment(t *testing.T, provider BacklogProvider, repo RepositoryRef, id string) string {
	t.Helper()
	comments, err := provider.ListComments(context.Background(), repo, id)
	if err != nil || len(comments) != 1 {
		t.Fatalf("ListComments = %+v, %v; want one", comments, err)
	}
	return comments[0].Body
}
