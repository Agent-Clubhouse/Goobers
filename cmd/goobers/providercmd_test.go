package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/providers"
)

// fakeIssue/fakePR are the fake GitHub server's in-memory records for
// #131/#132's CLI-level integration tests — a minimal, self-contained stand-in
// for the real api.github.com surface backlog-query/open-pr/issue-close-out
// actually talk to, scoped to exactly the endpoints those subcommands hit.
type fakeIssue struct {
	number   int
	title    string
	body     string
	labels   []string
	state    string
	comments []string
}

type fakePR struct {
	number     int
	title      string
	body       string
	head       string
	base       string
	headSHA    string
	baseSHA    string
	draft      bool
	labels     []string
	checkState string
	files      []fakePRFile
	state      string
}

// fakePRFile is one file a fakePR touches, for the /pulls/{id}/files endpoint
// ListPullRequestFiles reads (issue #359's sibling-set context gathering).
type fakePRFile struct {
	path      string
	status    string
	additions int
	deletions int
}

// fakeGitHubServer is a stateful httptest.Server standing in for GitHub's
// issues/comments/labels/pulls API, shared across #131/#132's CLI-level
// integration tests.
type fakeGitHubServer struct {
	mu     sync.Mutex
	owner  string
	repo   string
	issues map[int]*fakeIssue
	prs    map[int]*fakePR
	nextPR int
	server *httptest.Server
}

func newFakeGitHubServer(t *testing.T, owner, repo string) *fakeGitHubServer {
	t.Helper()
	s := &fakeGitHubServer{owner: owner, repo: repo, issues: map[int]*fakeIssue{}, prs: map[int]*fakePR{}, nextPR: 1}
	mux := http.NewServeMux()
	prefix := "/repos/" + owner + "/" + repo
	mux.HandleFunc(prefix+"/issues", s.handleIssuesCollection)
	mux.HandleFunc(prefix+"/pulls", s.handlePullsCollection)
	mux.HandleFunc(prefix+"/issues/", s.handleIssueItem)
	mux.HandleFunc(prefix+"/pulls/", s.handlePullItem)
	mux.HandleFunc(prefix+"/commits/", s.handleCommitItem)
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *fakeGitHubServer) addIssue(number int, title string, labels ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[number] = &fakeIssue{number: number, title: title, labels: append([]string{}, labels...), state: "open"}
}

// addOpenPR seeds a fixture PR for ListPullRequests/PullRequestFiles (issue
// #359) — distinct from handlePullsCollection's POST path (which models
// open-pr opening a fresh PR), this stands in for a PR that's already open
// when merge-review's selection stage runs. checkState defaults to "success"
// (GitHub's own vocabulary, normalized by combinedCheckState) when empty.
// Every fixture PR needs a matching addIssue with the same number too:
// UpdateWorkItem (labels/comments) always addresses the issues API, since
// GitHub PRs are issues under the hood.
func (s *fakeGitHubServer) addOpenPR(number int, head, base, headSHA, baseSHA string, draft bool, labels []string, files []fakePRFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prs[number] = &fakePR{
		number: number, head: head, base: base, headSHA: headSHA, baseSHA: baseSHA,
		draft: draft, labels: append([]string{}, labels...), checkState: "success",
		files: files, state: "open",
	}
	if number >= s.nextPR {
		s.nextPR = number + 1
	}
}

func (s *fakeGitHubServer) newGitHubProvider(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
	return providers.NewGitHubProvider(token, append(opts, func(p *providers.GitHubProvider) { p.BaseURL = s.server.URL })...)
}

func (s *fakeGitHubServer) handleIssuesCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var wantLabels []string
	if q := r.URL.Query().Get("labels"); q != "" {
		wantLabels = strings.Split(q, ",")
	}
	out := []map[string]interface{}{}
	for _, num := range sortedIntKeys(s.issues) {
		issue := s.issues[num]
		if !hasAllLabels(issue.labels, wantLabels) {
			continue
		}
		out = append(out, issueJSON(issue))
	}
	writeFakeJSON(w, out)
}

func (s *fakeGitHubServer) handleIssueItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/repos/"+s.owner+"/"+s.repo+"/issues/")
	parts := strings.Split(rest, "/")
	num, err := strconv.Atoi(parts[0])
	if err != nil {
		http.Error(w, "bad issue number", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	issue, ok := s.issues[num]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		writeFakeJSON(w, issueJSON(issue))
	case len(parts) == 1 && r.Method == http.MethodPatch:
		var body struct {
			Labels *[]string `json:"labels"`
			State  string    `json:"state"`
		}
		decodeFakeJSON(r, &body)
		if body.Labels != nil {
			issue.labels = *body.Labels
		}
		if body.State != "" {
			issue.state = body.State
		}
		writeFakeJSON(w, issueJSON(issue))
	case len(parts) == 2 && parts[1] == "comments" && r.Method == http.MethodGet:
		out := make([]map[string]interface{}, 0, len(issue.comments))
		for i, body := range issue.comments {
			out = append(out, map[string]interface{}{"id": i + 1, "body": body, "html_url": "", "user": map[string]string{"login": "goobers"}})
		}
		writeFakeJSON(w, out)
	case len(parts) == 2 && parts[1] == "comments" && r.Method == http.MethodPost:
		var body struct {
			Body string `json:"body"`
		}
		decodeFakeJSON(r, &body)
		issue.comments = append(issue.comments, body.Body)
		writeFakeJSON(w, map[string]interface{}{"id": len(issue.comments), "body": body.Body})
	case len(parts) == 2 && parts[1] == "labels" && r.Method == http.MethodPost:
		var body struct {
			Labels []string `json:"labels"`
		}
		decodeFakeJSON(r, &body)
		issue.labels = append(issue.labels, body.Labels...)
		writeFakeJSON(w, []map[string]string{})
	case len(parts) == 3 && parts[1] == "labels" && r.Method == http.MethodDelete:
		label := parts[2]
		kept := issue.labels[:0]
		for _, l := range issue.labels {
			if l != label {
				kept = append(kept, l)
			}
		}
		issue.labels = kept
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, fmt.Sprintf("unhandled %s %s", r.Method, r.URL.Path), http.StatusNotImplemented)
	}
}

func (s *fakeGitHubServer) handlePullsCollection(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		head := r.URL.Query().Get("head")
		base := r.URL.Query().Get("base")
		if head != "" {
			wantHead := strings.TrimPrefix(head, s.owner+":")
			out := []map[string]interface{}{}
			for _, num := range sortedPRKeys(s.prs) {
				pr := s.prs[num]
				if pr.state == "open" && pr.head == wantHead && (base == "" || pr.base == base) {
					out = append(out, prJSON(pr))
				}
			}
			writeFakeJSON(w, out)
			return
		}
		// No head filter: the ListPullRequests shape (issue #359) — the
		// provider applies its own client-side head-prefix filter, so this
		// fake just returns every open PR's full detail (draft/labels/
		// head+base sha), base-filtered.
		out := []map[string]interface{}{}
		for _, num := range sortedPRKeys(s.prs) {
			pr := s.prs[num]
			if pr.state == "open" && (base == "" || pr.base == base) {
				out = append(out, prDetailJSON(pr))
			}
		}
		writeFakeJSON(w, out)
	case http.MethodPost:
		var body struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			Head  string `json:"head"`
			Base  string `json:"base"`
		}
		decodeFakeJSON(r, &body)
		num := s.nextPR
		s.nextPR++
		s.prs[num] = &fakePR{number: num, title: body.Title, body: body.Body, head: body.Head, base: body.Base, state: "open"}
		writeFakeJSON(w, prJSON(s.prs[num]))
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

func (s *fakeGitHubServer) handlePullItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/repos/"+s.owner+"/"+s.repo+"/pulls/")
	parts := strings.Split(rest, "/")
	num, err := strconv.Atoi(parts[0])
	if err != nil {
		http.Error(w, "bad pr number", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pr, ok := s.prs[num]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch {
	case len(parts) == 2 && parts[1] == "files" && r.Method == http.MethodGet:
		out := make([]map[string]interface{}, 0, len(pr.files))
		for _, f := range pr.files {
			out = append(out, map[string]interface{}{
				"filename": f.path, "status": f.status, "additions": f.additions, "deletions": f.deletions,
			})
		}
		writeFakeJSON(w, out)
	case len(parts) == 1 && r.Method == http.MethodPatch:
		var body struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		decodeFakeJSON(r, &body)
		if body.Title != "" {
			pr.title = body.Title
		}
		if body.Body != "" {
			pr.body = body.Body
		}
		writeFakeJSON(w, prJSON(pr))
	default:
		http.Error(w, fmt.Sprintf("unhandled %s %s", r.Method, r.URL.Path), http.StatusNotImplemented)
	}
}

// handleCommitItem serves the legacy combined-status + check-runs endpoints
// GitHubProvider.combinedCheckState polls (BL-031), so ListPullRequests'
// per-candidate check state resolves against whichever fixture PR owns ref —
// looked up by matching headSHA since the fake has no separate commit store.
func (s *fakeGitHubServer) handleCommitItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/repos/"+s.owner+"/"+s.repo+"/commits/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		http.Error(w, "bad commit path", http.StatusBadRequest)
		return
	}
	sha, kind := parts[0], parts[1]
	s.mu.Lock()
	state := "success"
	for _, pr := range s.prs {
		if pr.headSHA == sha && pr.checkState != "" {
			state = pr.checkState
		}
	}
	s.mu.Unlock()
	switch kind {
	case "status":
		writeFakeJSON(w, map[string]interface{}{"state": state, "statuses": []map[string]interface{}{
			{"context": "ci", "state": state, "target_url": "", "description": ""},
		}})
	case "check-runs":
		writeFakeJSON(w, map[string]interface{}{"check_runs": []map[string]interface{}{}})
	default:
		http.Error(w, fmt.Sprintf("unhandled commit path %s", r.URL.Path), http.StatusNotImplemented)
	}
}

func issueJSON(issue *fakeIssue) map[string]interface{} {
	labels := make([]map[string]string, 0, len(issue.labels))
	for _, l := range issue.labels {
		labels = append(labels, map[string]string{"name": l})
	}
	return map[string]interface{}{
		"id": issue.number, "number": issue.number, "title": issue.title, "body": issue.body,
		"state": issue.state, "labels": labels, "html_url": fmt.Sprintf("https://example/issues/%d", issue.number),
	}
}

func prJSON(pr *fakePR) map[string]interface{} {
	return map[string]interface{}{
		"id": pr.number, "number": pr.number, "title": pr.title, "body": pr.body,
		"state": pr.state, "merged": false,
		"html_url": fmt.Sprintf("https://example/pull/%d", pr.number),
	}
}

// prDetailJSON is the ListPullRequests shape (issue #359): draft flag,
// labels, and head/base ref+sha, none of which prJSON's open-pr shape needs.
func prDetailJSON(pr *fakePR) map[string]interface{} {
	labels := make([]map[string]string, 0, len(pr.labels))
	for _, l := range pr.labels {
		labels = append(labels, map[string]string{"name": l})
	}
	return map[string]interface{}{
		"number": pr.number, "html_url": fmt.Sprintf("https://example/pull/%d", pr.number),
		"draft": pr.draft, "updated_at": "2026-07-15T00:00:00Z",
		"head":   map[string]interface{}{"ref": pr.head, "sha": pr.headSHA},
		"base":   map[string]interface{}{"ref": pr.base, "sha": pr.baseSHA},
		"labels": labels,
	}
}

func hasAllLabels(have, want []string) bool {
	for _, w := range want {
		if w == "" {
			continue
		}
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func sortedIntKeys(m map[int]*fakeIssue) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func sortedPRKeys(m map[int]*fakePR) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func writeFakeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func decodeFakeJSON(r *http.Request, out interface{}) {
	_ = json.NewDecoder(r.Body).Decode(out)
}
