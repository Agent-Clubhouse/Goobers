package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/goobers/goobers/providers"
)

// mootTestRepo is the fixture repo every case below runs against.
var mootTestRepo = providers.RepositoryRef{Owner: "your-org", Name: "your-repo"}

// newMootServer serves the two reads mootFailReason makes: the PR's changed
// files, and the state of each issue the PR's body says it closes.
//
// files == nil means "the files endpoint fails", which must be treated as
// unknown rather than as an empty diff — a provider error must never be read
// as evidence that a pull request changes nothing.
func newMootServer(t *testing.T, files []string, issueState map[string]string) *httptest.Server {
	t.Helper()
	prefix := "/repos/" + mootTestRepo.Owner + "/" + mootTestRepo.Name
	mux := http.NewServeMux()

	mux.HandleFunc(prefix+"/pulls/9/files", func(w http.ResponseWriter, r *http.Request) {
		if files == nil {
			// 404, not 5xx: classifyProviderError treats 5xx as retryable, so
			// a 5xx here would spend the provider's whole backoff schedule
			// before returning and make this test take ~15s for no reason.
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		out := make([]map[string]interface{}, 0, len(files))
		for _, f := range files {
			out = append(out, map[string]interface{}{"filename": f, "status": "modified"})
		}
		writeFakeJSON(w, out)
	})

	for id, state := range issueState {
		id, state := id, state
		mux.HandleFunc(prefix+"/issues/"+id, func(w http.ResponseWriter, r *http.Request) {
			if state == "" {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			n, _ := strconv.Atoi(id)
			writeFakeJSON(w, map[string]interface{}{
				"number": n, "state": state, "title": "t",
				"html_url": "https://github.com/x/y/issues/" + id,
			})
		})
	}

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func mootProvider(t *testing.T, files []string, issueState map[string]string) *providers.GitHubProvider {
	t.Helper()
	server := newMootServer(t, files, issueState)
	return providers.NewGitHubProvider("test-token", func(p *providers.GitHubProvider) { p.BaseURL = server.URL })
}

// TestMootFailReason covers the whole decision surface of #923's narrow
// auto-close.
//
// The design constraint being pinned here is that mootness rests ONLY on a
// deterministic fact about the repository — never on the reviewer's prose. A
// `fail` verdict is what makes the question worth asking; it is not evidence of
// the answer. Every ambiguous case must fail closed to the ordinary
// escalate-to-a-human path, because closing a pull request on a wrong belief is
// not a failure mode worth accepting to save a human one click.
func TestMootFailReason(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		files      []string
		issueState map[string]string
		wantMoot   bool
	}{
		// The general "already fixed elsewhere" shape, independent of any
		// issue bookkeeping: there is literally nothing left to merge.
		{
			name:     "empty diff is moot",
			body:     "Fixes #684",
			files:    []string{},
			wantMoot: true,
		},
		// PR #919's actual situation: #827 landed the real fix, #684 was
		// closed as superseded, and the run opened a PR for it 42 minutes
		// later anyway.
		{
			// PR #919's ACTUAL body shape. `goobers open-pr` writes
			// "Implements #N", which is not a GitHub closing keyword — so a
			// mootness check reading only closingKeywordPattern would miss the
			// single most common goober PR body form, including the one case
			// this path was written for.
			name:       "implements-form reference to a closed issue is moot",
			body:       "Implements #684: **flaky test: runexit_test.go intermittent**.",
			files:      []string{"cmd/goobers/runexit_test.go"},
			issueState: map[string]string{"684": "closed"},
			wantMoot:   true,
		},
		{
			name:       "all of several closing issues closed is moot",
			body:       "Fixes #684, closes #685",
			files:      []string{"a.go"},
			issueState: map[string]string{"684": "closed", "685": "closed"},
			wantMoot:   true,
		},
		// The ordinary `fail`: a real judgment call about the approach, which
		// stays with a human.
		{
			name:       "open closing issue is not moot",
			body:       "Fixes #684",
			files:      []string{"a.go"},
			issueState: map[string]string{"684": "open"},
			wantMoot:   false,
		},
		// One still-open issue is enough. This PR may still be the thing that
		// closes it, so the work is not obsolete.
		{
			name:       "one open among several closed is not moot",
			body:       "Fixes #684, closes #685",
			files:      []string{"a.go"},
			issueState: map[string]string{"684": "closed", "685": "open"},
			wantMoot:   false,
		},
		// No issue reference at all means there is no bookkeeping signal to
		// reason from — not that the work is obsolete.
		{
			name:     "no closing issue is not moot",
			body:     "A drive-by cleanup with no issue.",
			files:    []string{"a.go"},
			wantMoot: false,
		},
		// Fail closed on an unresolvable issue: a 404 is not evidence of
		// anything.
		{
			name:       "unresolvable issue is not moot",
			body:       "Fixes #684",
			files:      []string{"a.go"},
			issueState: map[string]string{"684": ""},
			wantMoot:   false,
		},
		// The sharpest fail-closed case. If the files read errors, we must NOT
		// fall through to "len(files) == 0 therefore empty diff" — that would
		// turn a transient provider blip into an automatic close of a pull
		// request whose diff nobody actually looked at.
		{
			name:       "files read failure is not an empty diff",
			body:       "Fixes #684",
			files:      nil,
			issueState: map[string]string{"684": "open"},
			wantMoot:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := mootProvider(t, tt.files, tt.issueState)
			pr := &providers.PullRequestSummary{Number: 9, Body: tt.body}
			reason, moot := mootFailReason(context.Background(), provider, mootTestRepo, pr)
			if moot != tt.wantMoot {
				t.Fatalf("mootFailReason = (%q, %v), want moot=%v", reason, moot, tt.wantMoot)
			}
			if moot && reason == "" {
				t.Fatal("moot with an empty reason — the reason is what justifies closing automatically and must be stated")
			}
			if !moot && reason != "" {
				t.Fatalf("not moot but got reason %q, want empty", reason)
			}
		})
	}
}

// TestResolvedAndClosingPatternsStayDistinct pins the separation between the
// two issue-reference patterns, which looks like duplication and is not.
//
// closingKeywordPattern (postmerge.go) drives a real mutation: post-merge
// CLOSES exactly the issues it matches when a PR lands. resolvedIssuePattern
// only ever answers "is this work already obsolete" and closes nothing, so it
// can afford to also recognize the "Implements #N" form `goobers open-pr`
// actually writes.
//
// Collapsing them in either direction is a bug:
//   - broaden closingKeywordPattern -> post-merge starts closing issues a PR
//     never claimed to close
//   - narrow resolvedIssuePattern -> mootness goes blind to the most common
//     goober PR body form, which is the #919 case
func TestResolvedAndClosingPatternsStayDistinct(t *testing.T) {
	const implementsForm = "Implements #684: **flaky test**."

	if got := closingIssueNumbers(implementsForm); len(got) != 0 {
		t.Errorf("closingIssueNumbers(%q) = %v, want none — 'Implements' is not a GitHub closing keyword, "+
			"and post-merge must not close an issue this PR never claimed to close", implementsForm, got)
	}
	if got := resolvedIssueNumbers(implementsForm); len(got) != 1 || got[0] != "684" {
		t.Errorf("resolvedIssueNumbers(%q) = %v, want [684] — this is the body shape open-pr actually writes", implementsForm, got)
	}

	// Both must still agree on genuine closing keywords.
	const closingForm = "Fixes #100, closes #101"
	if got := closingIssueNumbers(closingForm); len(got) != 2 {
		t.Errorf("closingIssueNumbers(%q) = %v, want 2", closingForm, got)
	}
	if got := resolvedIssueNumbers(closingForm); len(got) != 2 {
		t.Errorf("resolvedIssueNumbers(%q) = %v, want 2", closingForm, got)
	}
}
