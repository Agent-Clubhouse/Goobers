package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// fakeIssue/fakePR are the fake GitHub server's in-memory records for
// #131/#132's CLI-level integration tests — a minimal, self-contained stand-in
// for the real api.github.com surface backlog-query/open-pr/issue-close-out
// actually talk to, scoped to exactly the endpoints those subcommands hit.
type fakeIssue struct {
	number         int
	title          string
	body           string
	labels         []string
	state          string
	comments       []string
	commentIDs     []int64
	commentAuthors []string
	commentTypes   []string
	commentTimes   []time.Time
	assignee       string
	milestone      int
	children       []int
	blockers       []int
	createdAt      time.Time
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
	reviews    []fakeReview
	state      string
	merged     bool
	// selfReview, when set, makes POST /pulls/{n}/reviews return GitHub's
	// categorical self-review 422 — the #870 single-identity case where the
	// reviewing token is also the PR author.
	selfReview bool
}

type fakeReview struct {
	id        int64
	body      string
	commitSHA string
	state     string
}

// fakePRFile is one file a fakePR touches, for the /pulls/{id}/files endpoint
// ListPullRequestFiles reads (issue #359's sibling-set context gathering).
type fakePRFile struct {
	path      string
	status    string
	additions int
	deletions int
	// patch is the unified-diff hunk text; empty is valid for binary files
	// and tests that only care about file metadata.
	patch string
}

// fakeCompare is one fixture answer for GET .../compare/{base}...{head},
// registered explicitly per pair because the fake has no git history.
type fakeCompare struct {
	mergeBaseSHA string
	files        []fakePRFile
}

// fakeGitHubServer is a stateful httptest.Server standing in for GitHub's
// issues/comments/labels/pulls API, shared across #131/#132's CLI-level
// integration tests.
type fakeGitHubServer struct {
	mu       sync.Mutex
	owner    string
	repo     string
	issues   map[int]*fakeIssue
	prs      map[int]*fakePR
	compares map[string]fakeCompare
	contents map[string]string
	// branchTips answers GET .../git/ref/heads/<branch> (GitHubProvider.
	// BranchTipSHA) — the LIVE base-branch tip the merge-escalated self-heal
	// check (#1052) compares against, distinct from any PR's pinned baseSHA.
	branchTips    map[string]string
	nextPR        int
	nextCommentID int64
	server        *httptest.Server
	// filesRequests/checkStateRequests count GET /pulls/{n}/files and
	// /commits/{sha}/{status,check-runs} hits so cache tests can distinguish
	// memoized file lists from check states that must remain fresh.
	filesRequests      int
	checkStateRequests int
	pullListRequests   int
	dependencyRequests int
	authenticatedLogin string
}

// resetRequestCounts zeroes the per-endpoint counters between gather runs so
// a test can assert on one run's cost in isolation.
func (s *fakeGitHubServer) resetRequestCounts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filesRequests, s.checkStateRequests = 0, 0
}

func (s *fakeGitHubServer) requestCounts() (files, checkState int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.filesRequests, s.checkStateRequests
}

func (s *fakeGitHubServer) pullListRequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pullListRequests
}

func (s *fakeGitHubServer) dependencyRequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dependencyRequests
}

func newFakeGitHubServer(t *testing.T, owner, repo string) *fakeGitHubServer {
	t.Helper()
	s := &fakeGitHubServer{
		owner: owner, repo: repo, issues: map[int]*fakeIssue{}, prs: map[int]*fakePR{},
		compares: map[string]fakeCompare{}, contents: map[string]string{}, branchTips: map[string]string{},
		nextPR: 1, authenticatedLogin: "goobers",
	}
	mux := http.NewServeMux()
	prefix := "/repos/" + owner + "/" + repo
	mux.HandleFunc("/user", s.handleAuthenticatedUser)
	mux.HandleFunc(prefix+"/issues", s.handleIssuesCollection)
	mux.HandleFunc(prefix+"/pulls", s.handlePullsCollection)
	mux.HandleFunc(prefix+"/issues/", s.handleIssueItem)
	mux.HandleFunc(prefix+"/pulls/", s.handlePullItem)
	mux.HandleFunc(prefix+"/commits/", s.handleCommitItem)
	mux.HandleFunc(prefix+"/compare/", s.handleCompare)
	mux.HandleFunc(prefix+"/contents/", s.handleContents)
	mux.HandleFunc(prefix+"/git/ref/", s.handleGitRef)
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *fakeGitHubServer) setFileContent(ref, path, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contents[ref+"\x00"+path] = content
}

func (s *fakeGitHubServer) deleteFileContent(ref, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.contents, ref+"\x00"+path)
}

func (s *fakeGitHubServer) handleContents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/repos/"+s.owner+"/"+s.repo+"/contents/")
	ref := r.URL.Query().Get("ref")
	s.mu.Lock()
	content, ok := s.contents[ref+"\x00"+path]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "no content fixture registered for "+path+" at "+ref, http.StatusNotFound)
		return
	}
	if r.Header.Get("Accept") == "application/vnd.github.raw+json" {
		_, _ = w.Write([]byte(content))
		return
	}
	writeFakeJSON(w, map[string]string{
		"type": "file", "encoding": "base64",
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	})
}

func (s *fakeGitHubServer) handleAuthenticatedUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		return
	}
	writeFakeJSON(w, map[string]string{"login": s.authenticatedLogin})
}

func (s *fakeGitHubServer) addIssue(number int, title string, labels ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[number] = &fakeIssue{
		number: number, title: title, labels: append([]string{}, labels...), state: "open",
		createdAt: time.Now().UTC(),
	}
}

func (s *fakeGitHubServer) setIssueState(number int, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[number].state = state
}

func (s *fakeGitHubServer) setIssueBlockers(number int, blockers ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[number].blockers = append([]int(nil), blockers...)
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

// setBranchTip records the live tip SHA a branch's ref resolves to for
// GitHubProvider.BranchTipSHA (GET .../git/ref/heads/<branch>) — the base-
// advance signal the merge-escalated self-heal check reads (#1052). Distinct
// from a PR's pinned baseSHA: that is what GitHub freezes at PR-cut time; this
// is where the branch actually points now.
func (s *fakeGitHubServer) setBranchTip(branch, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.branchTips[branch] = sha
}

func (s *fakeGitHubServer) handleGitRef(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		return
	}
	prefix := "/repos/" + s.owner + "/" + s.repo + "/git/ref/"
	ref := strings.TrimPrefix(r.URL.Path, prefix) // e.g. "heads/main"
	branch := strings.TrimPrefix(ref, "heads/")
	s.mu.Lock()
	sha, ok := s.branchTips[branch]
	s.mu.Unlock()
	if !ok {
		// Unset on purpose: a test that reaches BranchTipSHA without seeding a
		// tip should fail loudly, not silently resolve to a phantom SHA.
		http.Error(w, "no branch tip registered for "+branch, http.StatusNotFound)
		return
	}
	writeFakeJSON(w, map[string]any{
		"ref":    "refs/" + ref,
		"object": map[string]string{"sha": sha, "type": "commit"},
	})
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
	q := r.URL.Query()
	var wantLabels []string
	if lq := q.Get("labels"); lq != "" {
		wantLabels = strings.Split(lq, ",")
	}
	// Model api.github.com's real list behavior rather than an idealized one
	// (#532): the issues list defaults to NEWEST-first (sort=created,
	// direction=desc) and paginates (per_page default 30, cap 100) with a
	// Link rel="next" header. The old fake returned everything, ascending, in
	// one page — exactly the idealization that let the FIFO fetch-window
	// starvation ship. Issue number stands in for created time (both fake and
	// real numbers ascend with creation).
	nums := sortedIntKeys(s.issues)
	if q.Get("direction") != "asc" {
		for i, j := 0, len(nums)-1; i < j; i, j = i+1, j-1 {
			nums[i], nums[j] = nums[j], nums[i]
		}
	}
	matched := []map[string]interface{}{}
	for _, num := range nums {
		issue := s.issues[num]
		if state := q.Get("state"); state != "" && state != "all" && issue.state != state {
			continue
		}
		if !hasAllLabels(issue.labels, wantLabels) {
			continue
		}
		matched = append(matched, issueJSON(issue))
	}
	perPage := 30
	if pp, err := strconv.Atoi(q.Get("per_page")); err == nil && pp > 0 {
		if pp > 100 {
			pp = 100
		}
		perPage = pp
	}
	page := 1
	if pg, err := strconv.Atoi(q.Get("page")); err == nil && pg > 0 {
		page = pg
	}
	start := (page - 1) * perPage
	if start > len(matched) {
		start = len(matched)
	}
	end := start + perPage
	if end > len(matched) {
		end = len(matched)
	}
	if end < len(matched) {
		next := *r.URL
		nq := next.Query()
		nq.Set("page", strconv.Itoa(page+1))
		nq.Set("per_page", strconv.Itoa(perPage))
		next.RawQuery = nq.Encode()
		w.Header().Set("Link", fmt.Sprintf("<%s%s>; rel=%q", s.server.URL, next.String(), "next"))
	}
	writeFakeJSON(w, matched[start:end])
}

func (s *fakeGitHubServer) handleIssueItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/repos/"+s.owner+"/"+s.repo+"/issues/")
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[0] == "comments" {
		s.handleCommentItem(w, r, parts[1])
		return
	}
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
			Labels    *[]string `json:"labels"`
			State     string    `json:"state"`
			Milestone *int      `json:"milestone"`
		}
		decodeFakeJSON(r, &body)
		if body.Labels != nil {
			issue.labels = *body.Labels
		}
		if body.State != "" {
			issue.state = body.State
		}
		if body.Milestone != nil {
			issue.milestone = *body.Milestone
		}
		writeFakeJSON(w, issueJSON(issue))
	case len(parts) == 2 && parts[1] == "sub_issues" && r.Method == http.MethodGet:
		out := make([]map[string]interface{}, 0, len(issue.children))
		for _, childID := range issue.children {
			if child, ok := s.issues[childID]; ok {
				out = append(out, issueJSON(child))
			}
		}
		writeFakeJSON(w, out)
	case len(parts) == 3 && parts[1] == "dependencies" && parts[2] == "blocked_by" && r.Method == http.MethodGet:
		s.dependencyRequests++
		out := make([]map[string]interface{}, 0, len(issue.blockers))
		for _, blockerID := range issue.blockers {
			if blocker, ok := s.issues[blockerID]; ok {
				out = append(out, issueJSON(blocker))
			}
		}
		writeFakeJSON(w, out)
	case len(parts) == 2 && parts[1] == "comments" && r.Method == http.MethodGet:
		out := make([]map[string]interface{}, 0, len(issue.comments))
		for i, body := range issue.comments {
			comment := map[string]interface{}{
				"id": issue.commentIDs[i], "body": body, "html_url": "",
				"user": map[string]string{"login": issue.commentAuthors[i], "type": issue.commentTypes[i]},
			}
			if !issue.commentTimes[i].IsZero() {
				comment["created_at"] = issue.commentTimes[i]
			}
			out = append(out, comment)
		}
		writeFakeJSON(w, out)
	case len(parts) == 2 && parts[1] == "comments" && r.Method == http.MethodPost:
		var body struct {
			Body string `json:"body"`
		}
		decodeFakeJSON(r, &body)
		s.nextCommentID++
		issue.comments = append(issue.comments, body.Body)
		issue.commentIDs = append(issue.commentIDs, s.nextCommentID)
		issue.commentAuthors = append(issue.commentAuthors, s.authenticatedLogin)
		issue.commentTypes = append(issue.commentTypes, "Bot")
		issue.commentTimes = append(issue.commentTimes, time.Now().UTC())
		writeFakeJSON(w, map[string]interface{}{"id": s.nextCommentID, "body": body.Body})
	case len(parts) == 2 && parts[1] == "labels" && r.Method == http.MethodPost:
		var body struct {
			Labels []string `json:"labels"`
		}
		decodeFakeJSON(r, &body)
		issue.labels = append(issue.labels, body.Labels...)
		writeFakeJSON(w, []map[string]string{})
	case len(parts) >= 3 && parts[1] == "labels" && r.Method == http.MethodDelete:
		label := strings.Join(parts[2:], "/")
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

func (s *fakeGitHubServer) handleCommentItem(w http.ResponseWriter, r *http.Request, idString string) {
	id, err := strconv.ParseInt(idString, 10, 64)
	if err != nil {
		http.Error(w, "bad comment id", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, number := range sortedIntKeys(s.issues) {
		issue := s.issues[number]
		for i, commentID := range issue.commentIDs {
			if commentID != id {
				continue
			}
			switch r.Method {
			case http.MethodPatch:
				var body struct {
					Body string `json:"body"`
				}
				decodeFakeJSON(r, &body)
				issue.comments[i] = body.Body
				writeFakeJSON(w, map[string]interface{}{"id": id, "body": body.Body})
			case http.MethodDelete:
				issue.comments = append(issue.comments[:i], issue.comments[i+1:]...)
				issue.commentIDs = append(issue.commentIDs[:i], issue.commentIDs[i+1:]...)
				issue.commentAuthors = append(issue.commentAuthors[:i], issue.commentAuthors[i+1:]...)
				issue.commentTypes = append(issue.commentTypes[:i], issue.commentTypes[i+1:]...)
				issue.commentTimes = append(issue.commentTimes[:i], issue.commentTimes[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unsupported", http.StatusMethodNotAllowed)
			}
			return
		}
	}
	http.Error(w, "comment not found", http.StatusNotFound)
}

func (s *fakeGitHubServer) handlePullsCollection(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		s.pullListRequests++
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
		// fake returns PRs in the requested state with full detail (draft/
		// labels/head+base sha), base-filtered.
		state := r.URL.Query().Get("state")
		if state == "" {
			state = "open"
		}
		out := []map[string]interface{}{}
		for _, num := range sortedPRKeys(s.prs) {
			pr := s.prs[num]
			if (state == "all" || pr.state == state) && (base == "" || pr.base == base) {
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
	case len(parts) == 1 && r.Method == http.MethodGet:
		writeFakeJSON(w, prDetailJSON(pr))
	case len(parts) == 2 && parts[1] == "reviews" && r.Method == http.MethodGet:
		out := make([]map[string]interface{}, 0, len(pr.reviews))
		for _, review := range pr.reviews {
			out = append(out, map[string]interface{}{
				"id": review.id, "body": review.body, "commit_id": review.commitSHA,
				"state": review.state, "html_url": fmt.Sprintf("https://example/pull/%d#review-%d", num, review.id),
				"user": map[string]string{"login": "goobers-reviewer"},
			})
		}
		writeFakeJSON(w, out)
	case len(parts) == 2 && parts[1] == "reviews" && r.Method == http.MethodPost:
		var body struct {
			Body     string `json:"body"`
			CommitID string `json:"commit_id"`
			Event    string `json:"event"`
		}
		decodeFakeJSON(r, &body)
		if pr.selfReview {
			// GitHub's exact categorical refusal (#870): author == reviewer.
			verb := "approve"
			if body.Event == "REQUEST_CHANGES" {
				verb = "request changes on"
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = fmt.Fprintf(w, `{"message":"Unprocessable Entity","errors":["Review Can not %s your own pull request"],"documentation_url":"https://docs.github.com/rest/pulls/reviews#create-a-review-for-a-pull-request"}`, verb)
			return
		}
		state := ""
		switch body.Event {
		case "APPROVE":
			state = "APPROVED"
		case "REQUEST_CHANGES":
			state = "CHANGES_REQUESTED"
		default:
			http.Error(w, "bad review event", http.StatusUnprocessableEntity)
			return
		}
		review := fakeReview{
			id: int64(len(pr.reviews) + 1), body: body.Body,
			commitSHA: body.CommitID, state: state,
		}
		pr.reviews = append(pr.reviews, review)
		writeFakeJSON(w, map[string]interface{}{
			"id": review.id, "body": review.body, "commit_id": review.commitSHA,
			"state": review.state, "html_url": fmt.Sprintf("https://example/pull/%d#review-%d", num, review.id),
		})
	case len(parts) == 2 && parts[1] == "files" && r.Method == http.MethodGet:
		s.filesRequests++
		out := make([]map[string]interface{}, 0, len(pr.files))
		for _, f := range pr.files {
			out = append(out, map[string]interface{}{
				"filename": f.path, "status": f.status, "additions": f.additions, "deletions": f.deletions, "patch": f.patch,
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
	s.checkStateRequests++
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

// handleCompare serves GET .../compare/{base}...{head} from the fixture map;
// an unregistered pair is a 404, matching a real "unknown ref" response.
func (s *fakeGitHubServer) handleCompare(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/repos/"+s.owner+"/"+s.repo+"/compare/")
	s.mu.Lock()
	cmp, ok := s.compares[key]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "no fixture compare registered for "+key, http.StatusNotFound)
		return
	}
	files := make([]map[string]interface{}, 0, len(cmp.files))
	for _, f := range cmp.files {
		files = append(files, map[string]interface{}{
			"filename": f.path, "status": f.status, "additions": f.additions, "deletions": f.deletions, "patch": f.patch,
		})
	}
	writeFakeJSON(w, map[string]interface{}{
		"merge_base_commit": map[string]interface{}{"sha": cmp.mergeBaseSHA},
		"files":             files,
	})
}

func issueJSON(issue *fakeIssue) map[string]interface{} {
	labels := make([]map[string]string, 0, len(issue.labels))
	for _, l := range issue.labels {
		labels = append(labels, map[string]string{"name": l})
	}
	out := map[string]interface{}{
		"id": issue.number, "number": issue.number, "title": issue.title, "body": issue.body,
		"state": issue.state, "labels": labels, "html_url": fmt.Sprintf("https://example/issues/%d", issue.number),
		"issue_dependencies_summary": map[string]int{"total_blocked_by": len(issue.blockers)},
	}
	if !issue.createdAt.IsZero() {
		out["created_at"] = issue.createdAt
	}
	if issue.assignee != "" {
		out["assignees"] = []map[string]string{{"login": issue.assignee}}
	}
	if issue.milestone > 0 {
		out["milestone"] = map[string]interface{}{
			"id": issue.milestone, "number": issue.milestone,
			"title": fmt.Sprintf("Milestone %d", issue.milestone),
		}
	}
	return out
}

func prJSON(pr *fakePR) map[string]interface{} {
	return map[string]interface{}{
		"id": pr.number, "number": pr.number, "title": pr.title, "body": pr.body,
		"state": pr.state, "merged": pr.merged,
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
		"state": pr.state, "merged": pr.merged, "draft": pr.draft,
		"updated_at": "2026-07-15T00:00:00Z", "body": pr.body,
		"head":   map[string]interface{}{"ref": pr.head, "sha": pr.headSHA},
		"base":   map[string]interface{}{"ref": pr.base, "sha": pr.baseSHA},
		"labels": labels,
	}
}

// setPRBody sets a fixture PR's body after addOpenPR — a separate setter
// rather than another positional param on addOpenPR (already long, shared by
// many tests) for the one caller (#414's open-PR eligibility backstop) that
// needs it.
func (s *fakeGitHubServer) setPRBody(number int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prs[number].body = body
}

// setPRSelfReview marks a fixture PR so POST .../reviews returns GitHub's
// self-review 422 — the #870 single-identity case (reviewing token authored
// the PR).
func (s *fakeGitHubServer) setPRSelfReview(number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prs[number].selfReview = true
}

// closeIssue flips a fixture issue's state to closed — for #552's
// backlog-query blocked-eligibility tests, which need to prove a recorded
// blocker's closure unblocks the item it gates.
func (s *fakeGitHubServer) closeIssue(number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[number].state = "closed"
}

// setPRHead models a push/rebase to a fixture PR between runs: a new head
// SHA and the file set the new head touches (#523's cache-invalidation
// tests).
func (s *fakeGitHubServer) setPRHead(number int, headSHA string, files []fakePRFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.prs[number]
	pr.headSHA = headSHA
	pr.files = files
	for i := range pr.reviews {
		if pr.reviews[i].state == "APPROVED" && pr.reviews[i].commitSHA != headSHA {
			pr.reviews[i].state = "DISMISSED"
		}
	}
}

// setPRBase models base advancing under an unchanged PR.
func (s *fakeGitHubServer) setPRBase(number int, baseSHA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prs[number].baseSHA = baseSHA
}

func (s *fakeGitHubServer) setPRDraft(number int, draft bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prs[number].draft = draft
}

func (s *fakeGitHubServer) setPRLabels(number int, labels []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prs[number].labels = append([]string(nil), labels...)
}

// setPRCheckState models CI advancing or rerunning on an unchanged head.
func (s *fakeGitHubServer) setPRCheckState(number int, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prs[number].checkState = state
}

// setPRClosed models a fixture PR closing without merging between runs.
func (s *fakeGitHubServer) setPRClosed(number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prs[number].state = "closed"
}

func (s *fakeGitHubServer) setPRMerged(number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prs[number].state = "closed"
	s.prs[number].merged = true
}

// addComment seeds a comment directly on issue/PR number's thread, bypassing
// the POST endpoint — for tests that need a fixture PR to already carry a
// prior run's posted verdict comment (#523's verdict-cache lookup) before
// the stage under test ever runs.
func (s *fakeGitHubServer) addComment(number int, body string) {
	s.addCommentAs(number, s.authenticatedLogin, body)
}

func (s *fakeGitHubServer) addCommentAs(number int, author, body string) {
	s.addCommentAtAs(number, author, body, time.Time{})
}

func (s *fakeGitHubServer) addCommentAtAs(number int, author, body string, createdAt time.Time) {
	s.addCommentAtAsType(number, author, "", body, createdAt)
}

func (s *fakeGitHubServer) addCommentAtAsType(number int, author, authorType, body string, createdAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextCommentID++
	s.issues[number].comments = append(s.issues[number].comments, body)
	s.issues[number].commentIDs = append(s.issues[number].commentIDs, s.nextCommentID)
	s.issues[number].commentAuthors = append(s.issues[number].commentAuthors, author)
	s.issues[number].commentTypes = append(s.issues[number].commentTypes, authorType)
	s.issues[number].commentTimes = append(s.issues[number].commentTimes, createdAt)
}

func (s *fakeGitHubServer) addChild(parent, child int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[parent].children = append(s.issues[parent].children, child)
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

// TestProviderBranchNamespace covers the cmd/goobers seam of the #965/#1010
// change: the run-branch namespace the runner injects (GOOBERS_BRANCH_NAMESPACE)
// becomes the default a PR-selector's headPrefix and a run-branch head derive
// from, defaulting to providers.DefaultBranchNamespace when unset (standalone
// use or a default-prefix gaggle).
func TestProviderBranchNamespace(t *testing.T) {
	t.Run("defaults when unset", func(t *testing.T) {
		t.Setenv(executor.BranchNamespaceEnvVar, "")
		if got := providerBranchNamespace(); got != providers.DefaultBranchNamespace {
			t.Errorf("providerBranchNamespace() = %q, want default %q", got, providers.DefaultBranchNamespace)
		}
	})
	t.Run("reads and normalizes the injected namespace", func(t *testing.T) {
		t.Setenv(executor.BranchNamespaceEnvVar, "acme") // no trailing slash
		if got, want := providerBranchNamespace(), "acme/"; got != want {
			t.Errorf("providerBranchNamespace() = %q, want %q", got, want)
		}
		// pr-select's implementation sub-namespace composes on top of it, so a
		// non-default gaggle selects goobers-analogous "acme/implementation/" PRs.
		if got, want := providerBranchNamespace()+"implementation/", "acme/implementation/"; got != want {
			t.Errorf("composed headPrefix = %q, want %q", got, want)
		}
	})
}
