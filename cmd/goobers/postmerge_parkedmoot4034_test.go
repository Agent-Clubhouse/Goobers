package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/providers"
)

// parkedMootTestRepo is the fixture repo every case below runs against.
var parkedMootTestRepo = providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

type parkedMootIssue struct {
	state  string
	reason string
}

// parkedMootServer is a minimal fake GitHub backend for the two things
// closeMootParkedPRsFrom needs beyond the []providers.PullRequestSummary
// passed directly (mirroring unparkResolvedSiblingsFrom's own *From test
// entrypoint): reading an issue's state/state_reason, and closing a PR with a
// comment.
type parkedMootServer struct {
	mu       sync.Mutex
	issues   map[string]parkedMootIssue
	closed   map[int]bool
	comments map[int]string
}

func newParkedMootProvider(t *testing.T, issues map[string]parkedMootIssue) (*providers.GitHubProvider, *parkedMootServer) {
	t.Helper()
	fixture := &parkedMootServer{issues: issues, closed: map[int]bool{}, comments: map[int]string{}}
	prefix := "/repos/" + parkedMootTestRepo.Owner + "/" + parkedMootTestRepo.Name
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, prefix+"/issues/"):
			fixture.issuesMux(w, r, prefix)
		case strings.HasPrefix(r.URL.Path, prefix+"/pulls/"):
			fixture.pullsMux(w, r, prefix)
		default:
			http.Error(w, fmt.Sprintf("unhandled %s %s", r.Method, r.URL.Path), http.StatusNotImplemented)
		}
	}))
	t.Cleanup(server.Close)
	return providers.NewGitHubProvider("test-token", func(p *providers.GitHubProvider) { p.BaseURL = server.URL }), fixture
}

func (s *parkedMootServer) issuesMux(w http.ResponseWriter, r *http.Request, prefix string) {
	rest := strings.TrimPrefix(r.URL.Path, prefix+"/issues/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if len(parts) == 2 && parts[1] == "comments" {
		if r.Method != http.MethodPost {
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Body string `json:"body"`
		}
		decodeFakeJSON(r, &body)
		n, _ := strconv.Atoi(id)
		s.mu.Lock()
		s.comments[n] = body.Body
		s.mu.Unlock()
		writeFakeJSON(w, map[string]interface{}{"id": 1, "body": body.Body})
		return
	}
	s.mu.Lock()
	issue, ok := s.issues[id]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	n, _ := strconv.Atoi(id)
	writeFakeJSON(w, map[string]interface{}{
		"number": n, "state": issue.state, "state_reason": issue.reason, "title": "t",
		"html_url": "https://github.com/x/y/issues/" + id,
	})
}

func (s *parkedMootServer) pullsMux(w http.ResponseWriter, r *http.Request, prefix string) {
	id := strings.TrimPrefix(r.URL.Path, prefix+"/pulls/")
	if r.Method != http.MethodPatch {
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		State string `json:"state"`
	}
	decodeFakeJSON(r, &body)
	n, _ := strconv.Atoi(id)
	if body.State == "closed" {
		s.mu.Lock()
		s.closed[n] = true
		s.mu.Unlock()
	}
	writeFakeJSON(w, map[string]interface{}{"number": n, "state": body.State, "merged": false})
}

func (s *parkedMootServer) wasClosed(number int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed[number]
}

func (s *parkedMootServer) commentFor(number int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.comments[number]
}

// TestIssueCompletedElsewhere pins the COMPLETED-vs-NOT_PLANNED distinction
// #4034 depends on: a NOT_PLANNED closure means the issue was dropped as out
// of scope, and the PR proposing to resolve it may still carry salvageable
// work, so only a COMPLETED closure may ever retire a parked PR.
func TestIssueCompletedElsewhere(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  bool
	}{
		{name: "closed completed", state: "completed", want: true},
		{name: "closed not planned", state: "not_planned", want: false},
		{name: "closed with no reason recorded", state: "", want: false},
		{name: "case-insensitive completed", state: "COMPLETED", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			item := providers.WorkItem{State: "closed", StateReason: tc.state}
			if got := issueCompletedElsewhere(item); got != tc.want {
				t.Fatalf("issueCompletedElsewhere(state_reason=%q) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
	if issueCompletedElsewhere(providers.WorkItem{State: "open", StateReason: "completed"}) {
		t.Fatal("an open issue must never be treated as completed regardless of state_reason")
	}
}

// TestCloseMootParkedPRsFrom is the #4034 regression: a bot PR parked
// needs-human (or needs-remediation / merge-escalated) whose entire reason to
// exist was already resolved — as COMPLETED, never NOT_PLANNED — by a
// different PR must be retired automatically instead of sitting forever,
// since pr-select's hard exclusion of these labels means the ordinary
// apply-verdict mootness check never runs on it again.
func TestCloseMootParkedPRsFrom(t *testing.T) {
	issues := map[string]parkedMootIssue{
		// #2746: completed by a different PR — #3908 below is moot.
		"2746": {state: "closed", reason: "completed"},
		// #900: dropped as out of scope — #901 below must stay parked; its
		// work may still be salvageable.
		"900": {state: "closed", reason: "not_planned"},
		// #950: still open — #951 below must stay parked.
		"950": {state: "open"},
	}
	provider, fixture := newParkedMootProvider(t, issues)

	others := []providers.PullRequestSummary{
		{Number: 3908, Body: "Closes #2746: some title.", Labels: []string{providers.LabelNeedsHuman, needsRemediationLabel}},
		{Number: 901, Body: "Closes #900: some title.", Labels: []string{providers.LabelNeedsHuman}},
		{Number: 951, Body: "Closes #950: some title.", Labels: []string{needsRemediationLabel}},
		// Parked but references no issue at all: never moot, regardless of label.
		{Number: 960, Body: "A drive-by cleanup with no issue.", Labels: []string{remediationEscalatedLabel}},
		// Not parked at all: untouched even though its issue is completed.
		{Number: 970, Body: "Closes #2746: some title.", Labels: nil},
	}

	closed, errs := closeMootParkedPRsFrom(context.Background(), provider, parkedMootTestRepo, 0, others, io.Discard, io.Discard)
	if len(errs) != 0 {
		t.Fatalf("closeMootParkedPRsFrom errs = %v, want none", errs)
	}
	if len(closed) != 1 || closed[0] != 3908 {
		t.Fatalf("closed = %v, want exactly [3908]", closed)
	}
	if !fixture.wasClosed(3908) {
		t.Fatal("pr #3908 was not closed via PATCH state=closed")
	}
	if comment := fixture.commentFor(3908); !strings.Contains(comment, "#2746") {
		t.Fatalf("close comment for #3908 = %q, want it to name #2746", comment)
	}
	for _, number := range []int{901, 951, 960, 970} {
		if fixture.wasClosed(number) {
			t.Fatalf("pr #%d was closed, want left parked", number)
		}
	}
}

// TestParkedPRMootReasonFailsClosedOnUnresolvableIssue mirrors
// mootFailReason's own posture (applyverdict_moot923_test.go): a provider
// error resolving a referenced issue must never be read as "therefore moot".
func TestParkedPRMootReasonFailsClosedOnUnresolvableIssue(t *testing.T) {
	provider, _ := newParkedMootProvider(t, map[string]parkedMootIssue{})
	pr := providers.PullRequestSummary{Number: 1, Body: "Closes #999."}
	reason, moot := parkedPRMootReason(context.Background(), provider, parkedMootTestRepo, pr)
	if moot {
		t.Fatalf("parkedPRMootReason = (%q, true), want moot=false for an unresolvable issue", reason)
	}
}
