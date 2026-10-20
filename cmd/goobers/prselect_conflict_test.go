package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestPRSelectGitHubConflictRouting(t *testing.T) {
	for _, tc := range []struct {
		name, hold                  string
		advisory, targeted, failing bool
	}{
		{name: "managed"}, {name: "targeted", targeted: true}, {name: "failing checks", failing: true},
		{name: "advisory", advisory: true}, {name: "human hold", hold: providers.LabelNeedsHuman},
		{name: "escalated", hold: remediationEscalatedLabel}, {name: "demoted", hold: mergeDemotedLabel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			labels := []string{}
			if tc.hold != "" {
				labels = append(labels, tc.hold)
			}
			head := "goobers/implementation/conflict"
			if tc.advisory {
				head = "human/feature"
			}
			server.addIssue(42, "Conflicted PR", labels...)
			server.addOpenPR(42, head, "main", "head-42", "base", false, labels, nil)
			server.setPRMergeable(42, false)
			if tc.failing {
				server.setPRCheckState(42, "failure")
			}
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "conflict-selection")
			t.Setenv("GOOBERS_INPUT_AUTHORSCOPE", "any")
			t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", "custom:hold")
			if tc.targeted {
				t.Setenv("GOOBERS_TRIGGER_REF", "github-webhook:pull_request#42")
			}
			t.Chdir(t.TempDir())
			for range 2 {
				code, out, errOut := runArgs(t, "pr-select", root)
				if code != 0 || !strings.Contains(out, "no work") {
					t.Fatalf("code=%d stdout=%s stderr=%s", code, out, errOut)
				}
			}
			server.mu.Lock()
			comments := append([]string(nil), server.issues[42].comments...)
			server.mu.Unlock()
			if tc.advisory || tc.hold != "" {
				assertFakeIssueLabels(t, server, 42, labels, []string{needsRemediationLabel})
				if len(comments) != 0 {
					t.Fatalf("read-only/held PR received comments: %q", comments)
				}
				return
			}
			assertFakeIssueLabels(t, server, 42, []string{needsRemediationLabel}, nil)
			if len(comments) != 1 || !strings.Contains(comments[0], "mergeable=false") || !strings.Contains(comments[0], "head-42") {
				t.Fatalf("handoff comments=%q", comments)
			}
			if !goobersBookkeepingComment(comments[0]) {
				t.Fatal("handoff note would retrigger shared-identity comment watch")
			}
			// A newer head can independently conflict; only its note is new.
			server.setPRLabels(42, nil)
			server.setPRHead(42, "head-next", nil)
			code, out, errOut := runArgs(t, "pr-select", root)
			if code != 0 {
				t.Fatalf("next head code=%d stdout=%s stderr=%s", code, out, errOut)
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			if len(server.issues[42].comments) != 2 {
				t.Fatalf("new head comments=%q", server.issues[42].comments)
			}
		})
	}
}

func TestPRSelectGitHubUnknownMergeabilityRemainsEligible(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addOpenPR(42, "goobers/implementation/unknown", "main", "head", "base", false, nil, nil)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "unknown-selection")
	t.Chdir(t.TempDir())
	code, out, errOut := runArgs(t, "pr-select", root)
	if code != 0 || !strings.Contains(out, "selected PR #42") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out, errOut)
	}
}

type conflictADOFixture struct {
	mu          sync.Mutex
	labels      []string
	comments    []string
	head        string
	status      string
	advisory    bool
	labelWrites int
}

func conflictADOSelectServer(t *testing.T, st *conflictADOFixture) *httptest.Server {
	t.Helper()
	base := adoPRSelectServer(t, "approved")
	const prefix = "/acme/project/_apis/git/repositories/web/pullrequests"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		labels := make([]map[string]any, 0, len(st.labels))
		for i, name := range st.labels {
			labels = append(labels, map[string]any{"id": strconv.Itoa(i + 1), "name": name})
		}
		switch r.URL.Path {
		case prefix + "/359/labels":
			if r.Method == http.MethodPost {
				var label struct {
					Name string `json:"name"`
				}
				if err := json.NewDecoder(r.Body).Decode(&label); err != nil {
					t.Error(err)
				}
				st.labels = append(st.labels, label.Name)
				st.labelWrites++
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "10", "name": label.Name})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"value": labels})
			}
			return
		case prefix + "/359/threads":
			if r.Method == http.MethodPost {
				var body struct {
					Comments []struct {
						Content string `json:"content"`
					} `json:"comments"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				st.comments = append(st.comments, body.Comments[0].Content)
				_ = json.NewEncoder(w).Encode(map[string]any{"id": len(st.comments), "comments": []map[string]any{{"id": 1, "content": body.Comments[0].Content, "commentType": "text"}}})
			} else {
				threads := []map[string]any{}
				for i, body := range st.comments {
					threads = append(threads, map[string]any{"id": i + 1, "status": "closed", "comments": []map[string]any{{"id": 1, "content": body, "commentType": "text"}}})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"value": threads})
			}
			return
		}
		recorder := httptest.NewRecorder()
		base.Config.Handler.ServeHTTP(recorder, r)
		if r.URL.Path == prefix || r.URL.Path == prefix+"/359" {
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Error(err)
			}
			pr := body
			if r.URL.Path == prefix {
				pr = body["value"].([]any)[0].(map[string]any)
			}
			pr["mergeStatus"] = st.status
			pr["labels"] = labels
			pr["lastMergeSourceCommit"] = map[string]string{"commitId": st.head}
			if st.advisory {
				pr["sourceRefName"] = "refs/heads/human/feature"
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPRSelectADOConflictRouting(t *testing.T) {
	for _, tc := range []struct {
		name, hold         string
		advisory, targeted bool
	}{
		{name: "managed"}, {name: "targeted", targeted: true}, {name: "advisory", advisory: true},
		{name: "human hold", hold: providers.LabelNeedsHuman}, {name: "escalated", hold: remediationEscalatedLabel},
		{name: "demoted", hold: mergeDemotedLabel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, repo := providerDispatchFixture(t, providers.ProviderADO)
			st := &conflictADOFixture{head: "h359", status: "conflicts", advisory: tc.advisory}
			if tc.hold != "" {
				st.labels = []string{tc.hold}
			}
			server := conflictADOSelectServer(t, st)
			adoPRSelectEnv(t, repo, server)
			t.Setenv("GOOBERS_INPUT_HEADPREFIXES", "goobers/tb-ado-implementation/")
			t.Setenv("GOOBERS_INPUT_AUTHORSCOPE", "any")
			t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", "custom:hold")
			if tc.targeted {
				t.Setenv("GOOBERS_TRIGGER_REF", "github-webhook:pull_request#359")
			}
			t.Chdir(t.TempDir())
			for range 2 {
				code, out, errOut := runArgs(t, "pr-select", root)
				if code != 0 || !strings.Contains(out, "no work") {
					t.Fatalf("code=%d stdout=%s stderr=%s", code, out, errOut)
				}
			}
			st.mu.Lock()
			defer st.mu.Unlock()
			if tc.advisory || tc.hold != "" {
				if len(st.comments) != 0 || st.labelWrites != 0 {
					t.Fatalf("read-only/held PR mutated: %+v", st)
				}
			} else if len(st.comments) != 1 || st.labelWrites != 1 || !strings.Contains(st.comments[0], "mergeStatus=conflicts") {
				t.Fatalf("handoff comments=%q label writes=%d", st.comments, st.labelWrites)
			}
		})
	}
}

type conflictTestProvider struct {
	fresh       providers.PullRequestSummary
	comments    []providers.Comment
	labelWrites int
	failLabel   bool
}

func (p *conflictTestProvider) GetPullRequest(context.Context, providers.RepositoryRef, string) (providers.PullRequestSummary, error) {
	return p.fresh, nil
}
func (p *conflictTestProvider) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return p.comments, nil
}
func (p *conflictTestProvider) UpdateWorkItem(_ context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	if req.Comment != "" {
		p.comments = append(p.comments, providers.Comment{Body: req.Comment})
	}
	if len(req.AddLabels) > 0 {
		p.labelWrites++
		if p.failLabel {
			p.failLabel = false
			return providers.WorkItem{}, errors.New("label write failed")
		}
		p.fresh.Labels = append(p.fresh.Labels, req.AddLabels...)
	}
	return providers.WorkItem{}, nil
}

type conflictTestSource struct {
	prSelectSource
	provider *conflictTestProvider
}

func (s conflictTestSource) conflictProvider() prSelectConflictProvider { return s.provider }

func TestPRSelectConflictRechecksHeadAndHumanHold(t *testing.T) {
	conflict := false
	original := providers.PullRequestSummary{Number: 42, State: "open", Head: "goobers/implementation/test", HeadSHA: "old", Mergeable: &conflict}
	for _, kind := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
		for _, tc := range []struct {
			name, head, hold string
			wantError        bool
		}{
			{name: "new head", head: "new", wantError: true},
			{name: "unknown head", wantError: true},
			{name: "new human hold", head: "old", hold: providers.LabelNeedsHuman},
			{name: "new escalation", head: "old", hold: remediationEscalatedLabel},
		} {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				p := &conflictTestProvider{fresh: original}
				p.fresh.HeadSHA = tc.head
				p.fresh.Labels = []string{tc.hold}
				excluded, err := excludePRSelectConflict(t.Context(), conflictTestSource{provider: p}, providers.RepositoryRef{Provider: kind}, original, true, prSelectSourceRequest{headPrefixes: []string{"goobers/implementation/"}})
				if !excluded || (err != nil) != tc.wantError {
					t.Fatalf("excluded=%v err=%v", excluded, err)
				}
				if len(p.comments) > 0 || p.labelWrites > 0 {
					t.Fatal("mutated a stale/held PR")
				}
			})
		}
	}
}

func TestPRSelectConflictRetriesLabelWithoutDuplicatingNote(t *testing.T) {
	conflict := false
	p := &conflictTestProvider{fresh: providers.PullRequestSummary{Number: 42, State: "open", Head: "goobers/implementation/test", HeadSHA: "head", Mergeable: &conflict}, failLabel: true}
	source := conflictTestSource{provider: p}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub}
	if _, err := excludePRSelectConflict(t.Context(), source, repo, p.fresh, true, prSelectSourceRequest{headPrefixes: []string{"goobers/implementation/"}}); err == nil {
		t.Fatal("failed label write was hidden")
	}
	for range 2 {
		if _, err := excludePRSelectConflict(t.Context(), source, repo, p.fresh, true, prSelectSourceRequest{headPrefixes: []string{"goobers/implementation/"}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.comments) != 1 || p.labelWrites != 2 {
		t.Fatalf("comments=%d label writes=%d", len(p.comments), p.labelWrites)
	}
}

func TestPRSelectConflictHonorsFreshConfiguredHold(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(42, "Conflicted PR")
	server.addOpenPR(42, "goobers/implementation/conflict", "main", "head-42", "base", false, nil, nil)
	server.setPRMergeable(42, false)
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "fresh-hold-review")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", "custom:hold")
	t.Chdir(t.TempDir())
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	source, gate, err := newPRSelectSources(root, repo)
	if err != nil {
		t.Fatal(err)
	}
	conflict := false
	p := &conflictTestProvider{fresh: providers.PullRequestSummary{Number: 42, State: "open", HeadSHA: "head-42", Head: "goobers/implementation/conflict", Base: "main", Labels: []string{"custom:hold"}, Mergeable: &conflict}}
	var out, stderr strings.Builder
	code := runPRSelectCore(root, repo, conflictTestSource{prSelectSource: source, provider: p}, gate, &out, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if len(p.comments) > 0 || p.labelWrites > 0 {
		t.Fatalf("fresh configured exclusion ignored: comments=%d labels=%d stdout=%s", len(p.comments), p.labelWrites, out.String())
	}
}

func TestPRSelectConflictRechecksConfiguredEligibility(t *testing.T) {
	conflict := false
	original := providers.PullRequestSummary{Number: 42, State: "open", Head: "goobers/implementation/test", Base: "main", HeadSHA: "head", Author: "bot", Labels: []string{"review-opt-in"}, Assignees: []string{"self"}, Mergeable: &conflict}
	policy := prSelectSourceRequest{headPrefixes: []string{"goobers/implementation/"}, excludeLabels: []string{"custom:hold"}, requiredOptInLabel: "review-opt-in", respectAssignee: true, selfIdentity: "self", identityFilters: providers.ListPullRequestsRequest{Author: "bot"}}
	for _, tc := range []struct {
		name   string
		change func(*providers.PullRequestSummary)
	}{
		{"configured hold", func(pr *providers.PullRequestSummary) { pr.Labels = append(pr.Labels, "custom:hold") }},
		{"opt-in removed", func(pr *providers.PullRequestSummary) { pr.Labels = nil }},
		{"assignee changed", func(pr *providers.PullRequestSummary) { pr.Assignees = []string{"other"} }},
		{"author filter changed", func(pr *providers.PullRequestSummary) { pr.Author = "other" }},
		{"branch changed", func(pr *providers.PullRequestSummary) { pr.Head = "human/feature" }},
		{"base changed", func(pr *providers.PullRequestSummary) { pr.Base = "release" }},
	} {
		for _, kind := range []providers.ProviderKind{providers.ProviderGitHub, providers.ProviderADO} {
			t.Run(string(kind)+"/"+tc.name, func(t *testing.T) {
				p := &conflictTestProvider{fresh: original}
				tc.change(&p.fresh)
				excluded, err := excludePRSelectConflict(t.Context(), conflictTestSource{provider: p}, providers.RepositoryRef{Provider: kind}, original, true, policy)
				if err != nil || !excluded {
					t.Fatalf("excluded=%v err=%v", excluded, err)
				}
				if len(p.comments) > 0 || p.labelWrites > 0 {
					t.Fatal("fresh eligibility change was ignored")
				}
			})
		}
	}
}

func TestPRSelectConflictFreshIdentityUsesProviderSemantics(t *testing.T) {
	conflict := false
	original := providers.PullRequestSummary{
		Number: 42, State: "open", Head: "goobers/implementation/test", Base: "main",
		HeadSHA: "head", Author: "Author@Example.COM", RequestedReviewers: []string{"Reviewer@Example.COM"},
		Mergeable: &conflict,
	}
	for _, tc := range []struct {
		name     string
		provider providers.ProviderKind
		author   string
		reviewer string
		want     int
	}{
		{"ADO mixed-case UPNs", providers.ProviderADO, "author@example.com", "reviewer@example.com", 1},
		{"ADO changed author", providers.ProviderADO, "other@example.com", "reviewer@example.com", 0},
		{"ADO changed reviewer", providers.ProviderADO, "author@example.com", "other@example.com", 0},
		{"GitHub retains exact matching", providers.ProviderGitHub, "author@example.com", "reviewer@example.com", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &conflictTestProvider{fresh: original}
			policy := prSelectSourceRequest{
				headPrefixes:    []string{"goobers/implementation/"},
				identityFilters: providers.ListPullRequestsRequest{Author: tc.author, RequestedReviewer: tc.reviewer},
			}
			for range 2 {
				excluded, err := excludePRSelectConflict(t.Context(), conflictTestSource{provider: p},
					providers.RepositoryRef{Provider: tc.provider}, original, true, policy)
				if err != nil || !excluded {
					t.Fatalf("excluded=%v err=%v", excluded, err)
				}
			}
			if len(p.comments) != tc.want || p.labelWrites != tc.want {
				t.Fatalf("comments=%d label writes=%d; want %d each", len(p.comments), p.labelWrites, tc.want)
			}
			if tc.want > 0 && !goobersBookkeepingComment(p.comments[0].Body) {
				t.Fatal("conflict handoff lost its bookkeeping provenance")
			}
		})
	}
}
