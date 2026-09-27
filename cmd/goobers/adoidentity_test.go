package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// TestADOCommentAuthoredByComparesIdentityGUID pins the ADO-N5 "is this me"
// rule: a comment carrying an author GUID is self only when the GUID matches,
// whatever its display name; a comment without one falls back to the display
// name.
func TestADOCommentAuthoredByComparesIdentityGUID(t *testing.T) {
	self := providers.ADOIdentity{ID: "self-guid", DisplayName: "goobers-bot"}
	for _, tc := range []struct {
		name    string
		comment providers.Comment
		want    bool
	}{
		{name: "same guid same name", comment: providers.Comment{Author: "goobers-bot", AuthorID: "self-guid"}, want: true},
		{name: "same guid renamed", comment: providers.Comment{Author: "old-name", AuthorID: "SELF-GUID"}, want: true},
		{name: "shared name other guid", comment: providers.Comment{Author: "goobers-bot", AuthorID: "other-guid"}, want: false},
		{name: "no guid same name", comment: providers.Comment{Author: "goobers-bot"}, want: true},
		{name: "no guid other name", comment: providers.Comment{Author: "someone-else"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := adoCommentAuthoredBy(tc.comment, self); got != tc.want {
				t.Fatalf("adoCommentAuthoredBy(%+v) = %v, want %v", tc.comment, got, tc.want)
			}
		})
	}
	if adoCommentAuthoredBy(providers.Comment{Author: "goobers-bot", AuthorID: "self-guid"}, providers.ADOIdentity{DisplayName: "goobers-bot"}) {
		t.Fatal("a comment with an author GUID matched an identity with no GUID")
	}
}

// adoIdentityThreadsServer serves one PR's threads and a connectionData
// identity (self-guid, displayed as goobers-bot) for the ADO-N5 tests.
func adoIdentityThreadsServer(t *testing.T, comments []map[string]any) *providers.ADOProvider {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42/threads", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{"id": 7, "comments": comments}}})
	})
	mux.HandleFunc("/org/_apis/connectionData", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authenticatedUser": map[string]any{"id": "self-guid", "providerDisplayName": "goobers-bot"},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return providers.NewADOProvider("org", "project", "token", func(p *providers.ADOProvider) { p.BaseURL = server.URL })
}

// TestGatherPRContextADOTrustsVerdictByIdentityGUID proves the ADO
// gather-pr-context comment read attributes threads by identity GUID: a
// verdict from another identity that shares the display name is not trusted,
// and a verdict this identity wrote under an earlier display name is.
func TestGatherPRContextADOTrustsVerdictByIdentityGUID(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}
	impostor := renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "impostor"})
	own := renderVerdictComment(apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "own"})

	provider := adoIdentityThreadsServer(t, []map[string]any{
		{"id": 1, "commentType": "text", "content": impostor, "author": map[string]string{"displayName": "goobers-bot", "id": "other-guid"}},
	})
	comments, err := adoSelfAttributedThreadComments(context.Background(), provider, repo, "42")
	if err != nil {
		t.Fatalf("adoSelfAttributedThreadComments: %v", err)
	}
	if got := gatherPRVerdict("", repo, 42, comments, "goobers-bot"); got != nil {
		t.Fatalf("verdict = %+v, want nil: a shared display name must not be trusted", got)
	}

	provider = adoIdentityThreadsServer(t, []map[string]any{
		{"id": 1, "commentType": "text", "content": impostor, "author": map[string]string{"displayName": "goobers-bot", "id": "other-guid"}},
		{"id": 2, "commentType": "text", "content": own, "author": map[string]string{"displayName": "old-bot-name", "id": "self-guid"}},
	})
	comments, err = adoSelfAttributedThreadComments(context.Background(), provider, repo, "42")
	if err != nil {
		t.Fatalf("adoSelfAttributedThreadComments: %v", err)
	}
	got := gatherPRVerdict("", repo, 42, comments, "goobers-bot")
	if got == nil || got.Summary != "own" {
		t.Fatalf("verdict = %+v, want the verdict this identity wrote", got)
	}
}

// TestRecoverADOPassVerdictMatchesIdentityGUID proves merge-pr's pre-lock
// verdict recovery trusts the author GUID, not the display name: a later pass
// from another identity sharing the display name is ignored, and this
// identity's own pass is recovered and attributed to its current display name.
func TestRecoverADOPassVerdictMatchesIdentityGUID(t *testing.T) {
	repo := providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}
	provider := adoIdentityThreadsServer(t, []map[string]any{
		{"id": 1, "commentType": "text", "content": renderVerdictComment(apiv1.Verdict{
			Decision: apiv1.VerdictPass, Summary: "own pass", HeadSHA: "h", BaseSHA: "b",
		}), "author": map[string]string{"displayName": "old-bot-name", "id": "self-guid"}},
		{"id": 2, "commentType": "text", "content": renderVerdictComment(apiv1.Verdict{
			Decision: apiv1.VerdictPass, Summary: "impostor pass", HeadSHA: "h", BaseSHA: "b",
		}), "author": map[string]string{"displayName": "goobers-bot", "id": "other-guid"}},
	})
	var stderr bytes.Buffer
	recovered := recoverADOPassVerdict(context.Background(), provider, repo, "42", &stderr)
	if recovered == nil {
		t.Fatalf("recovered = nil, want this identity's pass; stderr = %q", stderr.String())
	}
	if recovered.Verdict.Summary != "own pass" {
		t.Fatalf("recovered summary = %q, want %q", recovered.Verdict.Summary, "own pass")
	}
	if recovered.Author != "goobers-bot" {
		t.Fatalf("recovered author = %q, want goobers-bot", recovered.Author)
	}
}
