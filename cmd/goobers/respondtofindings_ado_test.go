package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// fakeADOResponseComment is one comment in a fake ADO pull-request thread.
type fakeADOResponseComment struct {
	id                 int
	content            string
	authorID, authorDN string
	deleted            bool
}

// fakeADOResponseThread is one fake ADO pull-request thread.
type fakeADOResponseThread struct {
	id       int
	status   string
	comments []*fakeADOResponseComment
}

// fakeADOResponseThreads serves the pull-request thread calls
// respond-to-findings makes on Azure DevOps for pull request 77.
type fakeADOResponseThreads struct {
	t       *testing.T
	mu      sync.Mutex
	threads []*fakeADOResponseThread
	deleted []string
	// posts and patches count thread creates and comment edits, so a test
	// can pin how many writes one run makes.
	posts, patches int
}

func (f *fakeADOResponseThreads) writeCounts() (posts, patches int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts, f.patches
}

func (f *fakeADOResponseThreads) addThread(content, authorID, authorDN string) {
	f.threads = append(f.threads, &fakeADOResponseThread{
		id: len(f.threads) + 1, status: "closed",
		comments: []*fakeADOResponseComment{{id: 1, content: content, authorID: authorID, authorDN: authorDN}},
	})
}

func (f *fakeADOResponseThreads) thread(id int) *fakeADOResponseThread {
	for _, thread := range f.threads {
		if thread.id == id {
			return thread
		}
	}
	return nil
}

func (f *fakeADOResponseThreads) server(repo providers.RepositoryRef) *httptest.Server {
	t := f.t
	pr := "/" + repo.Owner + "/" + repo.Project + "/_apis/git/repositories/" + repo.Name + "/pullrequests/77"
	mux := http.NewServeMux()
	mux.HandleFunc("/"+repo.Owner+"/_apis/connectionData", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]any{"authenticatedUser": map[string]any{"id": "self-guid", "providerDisplayName": "Goobers"}})
	})
	mux.HandleFunc(pr+"/threads", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			value := []map[string]any{}
			for _, thread := range f.threads {
				comments := []map[string]any{}
				for _, c := range thread.comments {
					if c.deleted {
						continue
					}
					comments = append(comments, map[string]any{
						"id": c.id, "parentCommentId": 0, "content": c.content, "commentType": "text",
						"author": map[string]string{"id": c.authorID, "displayName": c.authorDN},
					})
				}
				value = append(value, map[string]any{"id": thread.id, "status": thread.status, "comments": comments})
			}
			writeJSONResp(t, w, map[string]any{"value": value})
		case http.MethodPost:
			var body struct {
				Comments []struct {
					Content string `json:"content"`
				} `json:"comments"`
				Status string `json:"status"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Comments) != 1 {
				t.Errorf("thread create body = %+v (err %v), want one comment", body, err)
			}
			if body.Status != "closed" {
				t.Errorf("thread status = %q, want closed so no comment-resolution policy trips", body.Status)
			}
			f.posts++
			f.addThread(body.Comments[0].Content, "self-guid", "Goobers")
			thread := f.threads[len(f.threads)-1]
			writeJSONResp(t, w, map[string]any{"id": thread.id, "comments": []map[string]any{{
				"id": 1, "content": body.Comments[0].Content, "commentType": "text",
				"author": map[string]string{"id": "self-guid", "displayName": "Goobers"},
			}}})
		default:
			t.Errorf("unexpected threads request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc(pr+"/threads/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest := strings.Split(strings.TrimPrefix(r.URL.Path, pr+"/threads/"), "/")
		if len(rest) != 3 || rest[1] != "comments" {
			t.Errorf("unexpected thread request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		threadID, _ := strconv.Atoi(rest[0])
		commentID, _ := strconv.Atoi(rest[2])
		thread := f.thread(threadID)
		if thread == nil || commentID != 1 {
			t.Errorf("request to unknown comment: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		comment := thread.comments[0]
		if comment.authorID != "self-guid" {
			t.Errorf("%s on thread %d, which another identity wrote", r.Method, threadID)
		}
		switch r.Method {
		case http.MethodPatch:
			f.patches++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			comment.content = body["content"]
			writeJSONResp(t, w, map[string]any{"id": commentID, "content": comment.content})
		case http.MethodDelete:
			comment.deleted = true
			f.deleted = append(f.deleted, rest[0])
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected comment request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected ADO request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux)
}

// ownResponses returns the live response comments this identity wrote.
func (f *fakeADOResponseThreads) ownResponses(runID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var bodies []string
	for _, thread := range f.threads {
		for _, c := range thread.comments {
			if !c.deleted && c.authorID == "self-guid" && strings.HasPrefix(c.content, remediationResponseMarker(runID)) {
				bodies = append(bodies, c.content)
			}
		}
	}
	return bodies
}

// respondToFindingsADOFixture routes respond-to-findings to an Azure DevOps
// repository with PR 77 claimed and a published remediation, with run
// attribution active as in a daemon run.
func respondToFindingsADOFixture(t *testing.T, runID string, verdict apiv1.Verdict, responses string) (string, providers.RepositoryRef) {
	t.Helper()
	root, repo := adoReviewThreadsStageFixture(t, runID)
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Setenv(executor.TaskEnvVar, "respond-to-findings")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), remediationResponseArtifactName))
	t.Chdir(t.TempDir())
	if _, err := claimPullRequestInOrder(root, repo, []providers.PullRequestSummary{{Number: 77}}, runID, "pr-remediation", time.Hour); err != nil {
		t.Fatalf("seed PR claim: %v", err)
	}
	seedRemediationResponseRun(t, root, runID, verdict, responses, true)
	return root, repo
}

// routeADOResponseProvider points the ADO stage provider at serverURL and
// asserts it is built from the declared github:issues:write credential.
func routeADOResponseProvider(t *testing.T, serverURL string) {
	t.Helper()
	original := newADOProviderForStage
	newADOProviderForStage = func(routed providers.RepositoryRef, credential providers.ADOCredentialSource) (*providers.ADOProvider, error) {
		if got := deliveredCapabilityOf(t, credential); got != string(capability.GitHubIssuesWrite) {
			t.Fatalf("respond-to-findings built its ADO provider from %q, want the declared %q", got, capability.GitHubIssuesWrite)
		}
		provider, err := buildADOProviderForStage(routed, credential)
		if err != nil {
			return nil, err
		}
		provider.BaseURL = serverURL
		return provider, nil
	}
	t.Cleanup(func() { newADOProviderForStage = original })
}

// TestRespondToFindingsOnADOPostsOneThread: the shipped pr-remediation runs
// respond-to-findings after every published remediation, so on Azure DevOps
// it posts its account as one closed pull-request thread. A retry updates
// that thread rather than posting another, and a thread from a different
// identity that shares the display name is never taken for its own. The
// stored comment carries the attribution footer, which must not make the run
// edit the thread it just wrote: one post on the first run, one edit on the
// retry.
func TestRespondToFindingsOnADOPostsOneThread(t *testing.T) {
	const runID = "ado-respond"
	verdict := apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{{Severity: apiv1.SeverityError, Class: apiv1.FindingSubstantive, Message: "validate empty input"}},
	}
	root, repo := respondToFindingsADOFixture(t, runID, verdict,
		`[{"finding":1,"disposition":"addressed","detail":"Added an explicit empty-input guard."}]`)
	fake := &fakeADOResponseThreads{t: t}
	impostor := remediationResponseMarker(runID) + "\nforged account"
	fake.addThread(impostor, "other-guid", "Goobers")
	server := fake.server(repo)
	defer server.Close()
	routeADOResponseProvider(t, server.URL)

	for attempt, want := range [][2]int{{1, 0}, {1, 1}} {
		if code, stdout, stderr := runArgs(t, "respond-to-findings", root); code != 0 {
			t.Fatalf("attempt %d: code = %d, stdout = %q, stderr = %q", attempt+1, code, stdout, stderr)
		}
		if posts, patches := fake.writeCounts(); posts != want[0] || patches != want[1] {
			t.Errorf("after attempt %d: thread posts/edits = %d/%d, want %d/%d", attempt+1, posts, patches, want[0], want[1])
		}
	}
	own := fake.ownResponses(runID)
	if len(own) != 1 {
		t.Fatalf("own response threads = %d, want one after a retry: %q", len(own), own)
	}
	if !strings.Contains(own[0], "1. **Addressed** - Added an explicit empty-input guard.") {
		t.Errorf("response = %q, want the addressed finding", own[0])
	}
	if attribution, ok, err := providers.ParseAttribution(own[0]); err != nil || !ok || attribution.Task != "respond-to-findings" {
		t.Errorf("response attribution = %+v, %v, %v; want the stage's attribution", attribution, ok, err)
	}
	if fake.threads[0].comments[0].content != impostor {
		t.Errorf("another identity's thread was modified: %q", fake.threads[0].comments[0].content)
	}
}

// TestRespondToFindingsOnADODeletesDuplicateThreads: when an earlier retry
// left two response threads, the first is kept and the duplicate deleted.
func TestRespondToFindingsOnADODeletesDuplicateThreads(t *testing.T) {
	const runID = "ado-respond-duplicates"
	root, repo := respondToFindingsADOFixture(t, runID, apiv1.Verdict{}, "")
	fake := &fakeADOResponseThreads{t: t}
	fake.addThread(remediationResponseMarker(runID)+"\nfirst", "self-guid", "Goobers")
	fake.addThread(remediationResponseMarker(runID)+"\nsecond", "self-guid", "Goobers")
	server := fake.server(repo)
	defer server.Close()
	routeADOResponseProvider(t, server.URL)

	if code, stdout, stderr := runArgs(t, "respond-to-findings", root); code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "2" {
		t.Fatalf("deleted threads = %v, want only the duplicate thread 2", fake.deleted)
	}
	if own := fake.ownResponses(runID); len(own) != 1 || !strings.Contains(own[0], "no merge-review findings") {
		t.Fatalf("own responses = %q, want the one updated account", own)
	}
}
