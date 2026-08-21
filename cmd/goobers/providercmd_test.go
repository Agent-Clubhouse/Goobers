package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
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
	updatedAt      time.Time
}

type fakeIssueEvent struct {
	id        int64
	number    int
	event     string
	label     string
	createdAt time.Time
}

type fakePR struct {
	number             int
	title              string
	body               string
	head               string
	base               string
	headSHA            string
	baseSHA            string
	draft              bool
	labels             []string
	checkState         string
	files              []fakePRFile
	reviews            []fakeReview
	author             string
	assignees          []string
	requestedReviewers []string
	state              string
	merged             bool
	// selfReview, when set, makes POST /pulls/{n}/reviews return GitHub's
	// categorical self-review 422 — the #870 single-identity case where the
	// reviewing token is also the PR author.
	selfReview bool
	// fineGrainedSelfReview returns the opaque 404 emitted for the same
	// restriction when the reviewer uses a fine-grained PAT.
	fineGrainedSelfReview bool
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
	path         string
	previousPath string
	status       string
	additions    int
	deletions    int
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
	branchTips map[string]string
	// repoLabels is the repository's label set (GET/POST /repos/o/r/labels),
	// distinct from any issue's applied labels. It lets a test model a label the
	// code names but the repository does not actually define — the #1801 failure,
	// where goobers:scope-gate-ack was referenced by a constant and existed
	// nowhere, so the escape hatch it advertised was unreachable.
	repoLabels    map[string]string
	nextPR        int
	nextCommentID int64
	nextEventID   int64
	issueEvents   []fakeIssueEvent
	server        *httptest.Server
	// filesRequests/checkStateRequests count GET /pulls/{n}/files and
	// /commits/{sha}/{status,check-runs} hits so cache tests can distinguish
	// memoized file lists from check states that must remain fresh.
	filesRequests      int
	checkStateRequests int
	issueListRequests  int
	issueListPageSizes []int
	pullListRequests   int
	dependencyRequests int
	authenticatedLogin string
	// issueEventRequests counts GET /repos/o/r/issues/events pages served, so a
	// test can price one backlog-health cycle's full-history walk against its
	// resumed successor (#3392).
	issueEventRequests int
	// issueEventQuotaLimit/Remaining, when set, put an absolute rate-limit
	// window on every repository issue-events page. quotaDrainPerPage models a
	// window that drains as the walk proceeds, which is what makes a
	// proactive floor observable at all.
	issueEventQuotaLimit     int
	issueEventQuotaRemaining int
	issueEventQuotaDrain     int
	// filesFailureStatus/filesFailureBody make GET /pulls/{n}/files fail with a
	// specific status/body instead of listing the PR's fixture files — used to
	// distinguish "the PR is gone" (the default 404 an unregistered number
	// already gets) from an unrelated transient failure on a PR that IS
	// registered, for classifier false-positive guards (#1770).
	filesFailureStatus map[int]int
	filesFailureBody   map[int]string
}

// setPullRequestFilesFailure makes GET /pulls/{number}/files respond with
// status/body instead of the PR's normal fixture file list. number must
// already be registered via addOpenPR (an unregistered number already 404s).
func (s *fakeGitHubServer) setPullRequestFilesFailure(number, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.filesFailureStatus == nil {
		s.filesFailureStatus = map[int]int{}
		s.filesFailureBody = map[int]string{}
	}
	s.filesFailureStatus[number] = status
	s.filesFailureBody[number] = body
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

func (s *fakeGitHubServer) issueListRequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issueListRequests
}

func (s *fakeGitHubServer) issueListPageSizeHistory() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.issueListPageSizes...)
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
		repoLabels: map[string]string{},
		nextPR:     1, authenticatedLogin: "goobers",
	}
	mux := http.NewServeMux()
	prefix := "/repos/" + owner + "/" + repo
	mux.HandleFunc("/user", s.handleAuthenticatedUser)
	mux.HandleFunc("/graphql", s.handleGraphQL)
	mux.HandleFunc(prefix+"/issues/events", s.handleIssueEvents)
	mux.HandleFunc(prefix+"/issues", s.handleIssuesCollection)
	mux.HandleFunc(prefix+"/pulls", s.handlePullsCollection)
	mux.HandleFunc(prefix+"/issues/", s.handleIssueItem)
	mux.HandleFunc(prefix+"/pulls/", s.handlePullItem)
	mux.HandleFunc(prefix+"/commits/", s.handleCommitItem)
	mux.HandleFunc(prefix+"/compare/", s.handleCompare)
	mux.HandleFunc(prefix+"/contents/", s.handleContents)
	mux.HandleFunc(prefix+"/git/ref/", s.handleGitRef)
	mux.HandleFunc(prefix+"/labels", s.handleRepoLabels)
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *fakeGitHubServer) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Variables map[string]interface{} `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	repository := make(map[string]interface{})
	for variable, value := range request.Variables {
		if !strings.HasPrefix(variable, "ref") {
			continue
		}
		state := "PENDING"
		for _, pr := range s.prs {
			if pr.headSHA != fmt.Sprint(value) {
				continue
			}
			switch pr.checkState {
			case "success":
				state = "SUCCESS"
			case "failure", "error":
				state = "FAILURE"
			}
			break
		}
		repository["r"+strings.TrimPrefix(variable, "ref")] = map[string]interface{}{
			"statusCheckRollup": map[string]string{"state": state},
		}
	}
	writeFakeJSON(w, map[string]interface{}{"data": map[string]interface{}{"repository": repository}})
}

// handleRepoLabels serves the repository label set: GET lists it, POST defines a
// new one. Modelling this separately from issue labels is what lets a test
// distinguish "the code named a label" from "the repository defines it".
func (s *fakeGitHubServer) handleRepoLabels(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		names := make([]string, 0, len(s.repoLabels))
		for name := range s.repoLabels {
			names = append(names, name)
		}
		sort.Strings(names)
		labels := make([]map[string]string, 0, len(names))
		for _, name := range names {
			labels = append(labels, map[string]string{"name": name, "description": s.repoLabels[name]})
		}
		writeFakeJSON(w, labels)
	case http.MethodPost:
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad label body", http.StatusBadRequest)
			return
		}
		if _, exists := s.repoLabels[body["name"]]; exists {
			http.Error(w, "label already exists", http.StatusUnprocessableEntity)
			return
		}
		s.repoLabels[body["name"]] = body["description"]
		writeFakeJSON(w, body)
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

// addRepoLabel declares a label as already defined in the repository.
func (s *fakeGitHubServer) addRepoLabel(name, description string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repoLabels[name] = description
}

// hasRepoLabel reports whether the repository defines a label.
func (s *fakeGitHubServer) hasRepoLabel(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.repoLabels[name]
	return ok
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
	createdAt := time.Now().UTC()
	s.issues[number] = &fakeIssue{
		number: number, title: title, labels: append([]string{}, labels...), state: "open",
		createdAt: createdAt, updatedAt: createdAt,
	}
	for _, label := range labels {
		s.appendLabelEventLocked(number, label, true, createdAt)
	}
}

// setIssueUpdatedAt records an issue's live updatedAt (#2340's staleness
// check compares this against a PR's pinned implementation-time snapshot).
func (s *fakeGitHubServer) setIssueUpdatedAt(number int, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if issue := s.issues[number]; issue != nil {
		issue.updatedAt = at
	}
}

func (s *fakeGitHubServer) appendLabelEventLocked(number int, label string, added bool, at time.Time) {
	s.nextEventID++
	event := "unlabeled"
	if added {
		event = "labeled"
	}
	s.issueEvents = append(s.issueEvents, fakeIssueEvent{
		id: s.nextEventID, number: number, event: event, label: label, createdAt: at,
	})
}

func (s *fakeGitHubServer) setLabelEventTime(number int, label string, added bool, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	event := "unlabeled"
	if added {
		event = "labeled"
	}
	for i := len(s.issueEvents) - 1; i >= 0; i-- {
		if s.issueEvents[i].number == number && s.issueEvents[i].label == label && s.issueEvents[i].event == event {
			s.issueEvents[i].createdAt = at
			return
		}
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
	s.issueListRequests++
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
	s.issueListPageSizes = append(s.issueListPageSizes, perPage)
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

// handleIssueEvents serves the repository-wide issue-event history NEWEST
// FIRST, as GitHub does. The ordering is load-bearing for #3392: it is what
// lets a cursored walk stop at the first already-seen event instead of paging
// through the whole history every cycle.
func (s *fakeGitHubServer) handleIssueEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issueEventRequests++
	if s.issueEventQuotaLimit > 0 {
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(s.issueEventQuotaLimit))
		remaining := s.issueEventQuotaRemaining
		if remaining < 0 {
			remaining = 0
		}
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		s.issueEventQuotaRemaining -= s.issueEventQuotaDrain
	}
	newestFirst := make([]fakeIssueEvent, len(s.issueEvents))
	for i, event := range s.issueEvents {
		newestFirst[len(s.issueEvents)-1-i] = event
	}
	perPage := 30
	if pp, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && pp > 0 {
		if pp > 100 {
			pp = 100
		}
		perPage = pp
	}
	page := 1
	if pg, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && pg > 0 {
		page = pg
	}
	start := (page - 1) * perPage
	if start > len(newestFirst) {
		start = len(newestFirst)
	}
	end := start + perPage
	if end > len(newestFirst) {
		end = len(newestFirst)
	}
	if end < len(newestFirst) {
		next := *r.URL
		query := next.Query()
		query.Set("page", strconv.Itoa(page+1))
		query.Set("per_page", strconv.Itoa(perPage))
		next.RawQuery = query.Encode()
		w.Header().Set("Link", fmt.Sprintf("<%s%s>; rel=%q", s.server.URL, next.String(), "next"))
	}
	out := make([]map[string]any, 0, end-start)
	for _, event := range newestFirst[start:end] {
		issue, ok := s.issues[event.number]
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"id": event.id, "event": event.event, "created_at": event.createdAt,
			"label": map[string]string{"name": event.label},
			"issue": issueJSON(issue),
		})
	}
	writeFakeJSON(w, out)
}

// addLabelChurn appends n synthetic add/remove events for an unrelated label,
// padding the repository's event history so a full-history walk costs more than
// one page — the shape #3392 is about.
func (s *fakeGitHubServer) addLabelChurn(number, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := time.Now().UTC().Add(-time.Duration(n) * time.Minute)
	for i := 0; i < n; i++ {
		s.appendLabelEventLocked(number, "churn", i%2 == 0, base.Add(time.Duration(i)*time.Minute))
	}
}

// setIssueEventQuota puts an absolute rate-limit window on the repository
// issue-events endpoint, draining by drain on every page served.
func (s *fakeGitHubServer) setIssueEventQuota(limit, remaining, drain int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issueEventQuotaLimit, s.issueEventQuotaRemaining, s.issueEventQuotaDrain = limit, remaining, drain
}

func (s *fakeGitHubServer) issueEventRequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issueEventRequests
}

func (s *fakeGitHubServer) resetIssueEventRequestCount() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issueEventRequests = 0
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
			Assignees *[]string `json:"assignees"`
			Body      *string   `json:"body"`
			State     string    `json:"state"`
			Milestone *int      `json:"milestone"`
		}
		decodeFakeJSON(r, &body)
		if body.Body != nil {
			issue.body = *body.Body
			if pr := s.prs[num]; pr != nil {
				pr.body = *body.Body
			}
		}
		if body.Labels != nil {
			before := append([]string(nil), issue.labels...)
			issue.labels = *body.Labels
			for _, label := range before {
				if !hasAllLabels(issue.labels, []string{label}) {
					s.appendLabelEventLocked(num, label, false, time.Now().UTC())
				}
			}
			for _, label := range issue.labels {
				if !hasAllLabels(before, []string{label}) {
					s.appendLabelEventLocked(num, label, true, time.Now().UTC())
				}
			}
		}
		if body.State != "" {
			issue.state = body.State
		}
		if body.Assignees != nil {
			issue.assignee = ""
			if len(*body.Assignees) > 0 {
				issue.assignee = (*body.Assignees)[0]
			}
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
	case len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodGet:
		out := make([]map[string]any, 0)
		for _, event := range s.issueEvents {
			if event.number != num {
				continue
			}
			out = append(out, map[string]any{
				"id": event.id, "event": event.event, "created_at": event.createdAt,
				"label": map[string]string{"name": event.label},
			})
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
		for _, label := range body.Labels {
			if hasAllLabels(issue.labels, []string{label}) {
				continue
			}
			issue.labels = append(issue.labels, label)
			s.appendLabelEventLocked(num, label, true, time.Now().UTC())
		}
		writeFakeJSON(w, []map[string]string{})
	case len(parts) >= 3 && parts[1] == "labels" && r.Method == http.MethodDelete:
		label := strings.Join(parts[2:], "/")
		kept := issue.labels[:0]
		removed := false
		for _, l := range issue.labels {
			if l != label {
				kept = append(kept, l)
			} else {
				removed = true
			}
		}
		issue.labels = kept
		if removed {
			s.appendLabelEventLocked(num, label, false, time.Now().UTC())
		}
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
		if perPage, err := strconv.Atoi(r.URL.Query().Get("per_page")); err == nil && perPage > 0 {
			page := 1
			if requestedPage, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && requestedPage > 0 {
				page = requestedPage
			}
			start := min((page-1)*perPage, len(out))
			end := min(start+perPage, len(out))
			out = out[start:end]
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
		if pr.fineGrainedSelfReview {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"message":"Not Found","documentation_url":"https://docs.github.com/rest/pulls/reviews#create-a-review-for-a-pull-request"}`)
			return
		}
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
		if status, injected := s.filesFailureStatus[num]; injected {
			http.Error(w, s.filesFailureBody[num], status)
			return
		}
		s.filesRequests++
		out := make([]map[string]interface{}, 0, len(pr.files))
		for _, f := range pr.files {
			out = append(out, map[string]interface{}{
				"filename": f.path, "previous_filename": f.previousPath, "status": f.status,
				"additions": f.additions, "deletions": f.deletions, "patch": f.patch,
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
			"filename": f.path, "previous_filename": f.previousPath, "status": f.status,
			"additions": f.additions, "deletions": f.deletions, "patch": f.patch,
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
	if !issue.updatedAt.IsZero() {
		out["updated_at"] = issue.updatedAt
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
	assignees := make([]map[string]string, 0, len(pr.assignees))
	for _, assignee := range pr.assignees {
		assignees = append(assignees, map[string]string{"login": assignee})
	}
	requestedReviewers := make([]map[string]string, 0, len(pr.requestedReviewers))
	for _, reviewer := range pr.requestedReviewers {
		requestedReviewers = append(requestedReviewers, map[string]string{"login": reviewer})
	}
	return map[string]interface{}{
		"number": pr.number, "html_url": fmt.Sprintf("https://example/pull/%d", pr.number),
		"state": pr.state, "merged": pr.merged, "draft": pr.draft,
		"updated_at": "2026-07-15T00:00:00Z", "body": pr.body,
		"head":                map[string]interface{}{"ref": pr.head, "sha": pr.headSHA},
		"base":                map[string]interface{}{"ref": pr.base, "sha": pr.baseSHA},
		"user":                map[string]string{"login": pr.author},
		"assignees":           assignees,
		"requested_reviewers": requestedReviewers,
		"labels":              labels,
	}
}

func (s *fakeGitHubServer) setPRIdentities(number int, author string, assignees, requestedReviewers []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.prs[number]
	pr.author = author
	pr.assignees = assignees
	pr.requestedReviewers = requestedReviewers
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

func (s *fakeGitHubServer) setPRFineGrainedSelfReview(number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prs[number].fineGrainedSelfReview = true
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

// TestProviderBaseBranch covers #2087's seam: the gaggle's configured default
// branch the runner injects (GOOBERS_BASE_BRANCH) becomes the default every
// PR-lifecycle stage's "base" input resolves to, defaulting to "main" when
// unset (standalone use, or a gaggle whose branch is already "main").
func TestProviderBaseBranch(t *testing.T) {
	t.Run("defaults to main when unset", func(t *testing.T) {
		t.Setenv(executor.BaseBranchEnvVar, "")
		if got := providerBaseBranch(); got != "main" {
			t.Errorf("providerBaseBranch() = %q, want %q", got, "main")
		}
	})
	t.Run("reads the injected base branch", func(t *testing.T) {
		t.Setenv(executor.BaseBranchEnvVar, "release")
		if got, want := providerBaseBranch(), "release"; got != want {
			t.Errorf("providerBaseBranch() = %q, want %q", got, want)
		}
	})
}
