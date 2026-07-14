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
	number int
	title  string
	body   string
	head   string
	base   string
	state  string
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
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *fakeGitHubServer) addIssue(number int, title string, labels ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[number] = &fakeIssue{number: number, title: title, labels: append([]string{}, labels...), state: "open"}
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
		wantHead := strings.TrimPrefix(head, s.owner+":")
		out := []map[string]interface{}{}
		for _, num := range sortedPRKeys(s.prs) {
			pr := s.prs[num]
			if pr.state == "open" && pr.head == wantHead && (base == "" || pr.base == base) {
				out = append(out, prJSON(pr))
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
	num, err := strconv.Atoi(rest)
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
	if r.Method != http.MethodPatch {
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		return
	}
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
