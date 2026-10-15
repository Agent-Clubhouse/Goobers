package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// adoRefsFixture is the Git Refs and active pull-request surface of one fake
// Azure DevOps repository (org/project/repo): branch tips by name, active pull
// requests as (source, target) pairs, and every ref update it received.
type adoRefsFixture struct {
	mu   *sync.Mutex
	tips map[string]string
	prs  [][2]string
	// stale makes a ref update fail as staleOldObjectId; goneOnStale also
	// removes the ref first, modelling Azure DevOps' own deletion landing
	// between the read and the delete.
	stale, goneOnStale bool
	updates            []map[string]string
}

func newADORefsFixture(t *testing.T, f *adoRefsFixture) *ADOProvider {
	t.Helper()
	f.mu = &sync.Mutex{}
	mux := http.NewServeMux()
	base := "/org/project/_apis/git/repositories/repo"
	mux.HandleFunc(base+"/refs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodGet {
			prefix := strings.TrimPrefix(r.URL.Query().Get("filter"), "heads/")
			refs := []map[string]string{}
			for name, sha := range f.tips {
				if strings.HasPrefix(name, prefix) {
					refs = append(refs, map[string]string{"name": "refs/heads/" + name, "objectId": sha})
				}
			}
			writeJSON(t, w, map[string]any{"value": refs})
			return
		}
		var body []map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 {
			t.Errorf("ref update body = %v (%v)", body, err)
			return
		}
		update := body[0]
		f.updates = append(f.updates, update)
		name := strings.TrimPrefix(update["name"], "refs/heads/")
		if tip, ok := f.tips[name]; f.stale || !ok || tip != update["oldObjectId"] {
			if f.goneOnStale {
				delete(f.tips, name)
			}
			writeJSON(t, w, map[string]any{"value": []map[string]any{{"name": update["name"], "success": false, "updateStatus": "staleOldObjectId"}}})
			return
		}
		delete(f.tips, name)
		writeJSON(t, w, map[string]any{"value": []map[string]any{{"name": update["name"], "success": true}}})
	})
	mux.HandleFunc(base+"/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		target := r.URL.Query().Get("searchCriteria.targetRefName")
		prs := []map[string]any{}
		for i, pr := range f.prs {
			if target != "" && "refs/heads/"+pr[1] != target {
				continue
			}
			if r.URL.Query().Get("$skip") != "0" {
				continue
			}
			prs = append(prs, map[string]any{"pullRequestId": 100 + i, "status": "active",
				"sourceRefName": "refs/heads/" + pr[0], "targetRefName": "refs/heads/" + pr[1]})
		}
		writeJSON(t, w, map[string]any{"value": prs})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
}

const (
	cleanupBranch = "goobers/implementation/run-1"
	cleanupMerged = "1111111111111111111111111111111111111111"
	cleanupLater  = "2222222222222222222222222222222222222222"
	cleanupZero   = "0000000000000000000000000000000000000000"
)

// TestADOCleanupMergedSourceBranch is #5957's post-merge backstop: a branch
// Azure DevOps already removed is confirmed, one it left at the merged head is
// deleted conditionally on that head, and a branch that moved, or that another
// open pull request targets, is kept.
func TestADOCleanupMergedSourceBranch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fixture    adoRefsFixture
		want       MergedBranchCleanupStatus
		wantUpdate bool
		wantKept   bool
	}{
		{name: "azure devops already deleted it", fixture: adoRefsFixture{tips: map[string]string{cleanupBranch + "-sibling": cleanupLater}}, want: MergedBranchDeleted},
		{name: "left behind at the merged head", fixture: adoRefsFixture{tips: map[string]string{cleanupBranch: cleanupMerged}}, want: MergedBranchDeleted, wantUpdate: true},
		{name: "moved after the merge", fixture: adoRefsFixture{tips: map[string]string{cleanupBranch: cleanupLater}}, want: MergedBranchSkippedMoved, wantKept: true},
		{name: "stacked", fixture: adoRefsFixture{tips: map[string]string{cleanupBranch: cleanupMerged}, prs: [][2]string{{"goobers/child", cleanupBranch}}}, want: MergedBranchSkippedStacked, wantKept: true},
		{name: "lease lost to azure devops' own delete", fixture: adoRefsFixture{tips: map[string]string{cleanupBranch: cleanupMerged}, stale: true, goneOnStale: true}, want: MergedBranchDeleted, wantUpdate: true},
		{name: "lease lost to a push", fixture: adoRefsFixture{tips: map[string]string{cleanupBranch: cleanupMerged}, stale: true}, want: MergedBranchSkippedMoved, wantUpdate: true, wantKept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fixture
			provider := newADORefsFixture(t, &f)
			got, err := provider.CleanupMergedSourceBranch(context.Background(), adoLandingRepo(), cleanupBranch, cleanupMerged)
			if err != nil || got != tc.want {
				t.Fatalf("CleanupMergedSourceBranch = %q, %v; want %q", got, err, tc.want)
			}
			if (len(f.updates) > 0) != tc.wantUpdate {
				t.Fatalf("ref updates = %v, want update=%t", f.updates, tc.wantUpdate)
			}
			for _, update := range f.updates {
				if update["oldObjectId"] != cleanupMerged || update["newObjectId"] != cleanupZero {
					t.Fatalf("ref update = %v, want a delete conditional on the merged head", update)
				}
			}
			if _, kept := f.tips[cleanupBranch]; kept != tc.wantKept {
				t.Fatalf("branch kept = %t, want %t", kept, tc.wantKept)
			}
		})
	}
}

// TestADOCleanupMergedSourceBranchFailsClosedWithoutTheMergedHead: without
// the merged head a branch that moved after the merge cannot be told from the
// merged one, so nothing is read or deleted.
func TestADOCleanupMergedSourceBranchFailsClosedWithoutTheMergedHead(t *testing.T) {
	f := adoRefsFixture{tips: map[string]string{cleanupBranch: cleanupLater}}
	provider := newADORefsFixture(t, &f)
	if _, err := provider.CleanupMergedSourceBranch(context.Background(), adoLandingRepo(), cleanupBranch, ""); err == nil || !strings.Contains(err.Error(), "merged head") {
		t.Fatalf("error = %v, want a refusal naming the unknown merged head", err)
	}
	if _, err := provider.CleanupMergedSourceBranch(context.Background(), adoLandingRepo(), "", cleanupMerged); err == nil {
		t.Fatal("cleanup without a head branch succeeded")
	}
	if len(f.updates) != 0 || f.tips[cleanupBranch] != cleanupLater {
		t.Fatalf("updates = %v, tips = %v; want the branch untouched", f.updates, f.tips)
	}
}

// TestADODeleteUnusedBranch is terminal cleanup's ADO delete: one list of the
// active pull requests keeps a branch that is the base of a stacked pull
// request or the source of an open one (resumed or continued runs, or a
// person's pull request), and an unused branch is deleted conditionally on the
// tip it reads.
func TestADODeleteUnusedBranch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prs      [][2]string
		wantBase *bool
	}{
		{name: "unused"},
		{name: "base of a stacked pull request", prs: [][2]string{{"goobers/child", cleanupBranch}}, wantBase: ptrBool(true)},
		{name: "source of an open pull request", prs: [][2]string{{cleanupBranch, "main"}}, wantBase: ptrBool(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := adoRefsFixture{tips: map[string]string{cleanupBranch: cleanupLater}, prs: tc.prs}
			provider := newADORefsFixture(t, &f)
			result, err := provider.DeleteUnusedBranch(context.Background(), DeleteBranchRequest{Repository: adoLandingRepo(), Name: cleanupBranch})
			if tc.wantBase == nil {
				if err != nil || !result.Deleted {
					t.Fatalf("DeleteUnusedBranch = %+v, %v; want deleted", result, err)
				}
				if len(f.updates) != 1 || f.updates[0]["oldObjectId"] != cleanupLater || f.updates[0]["newObjectId"] != cleanupZero {
					t.Fatalf("ref updates = %v, want one delete conditional on the read tip", f.updates)
				}
				return
			}
			var inUse *BranchInUseError
			if !errors.As(err, &inUse) || inUse.AsBase != *tc.wantBase || inUse.Name != cleanupBranch {
				t.Fatalf("error = %v, want BranchInUseError{AsBase: %t}", err, *tc.wantBase)
			}
			if len(f.updates) != 0 {
				t.Fatalf("ref updates = %v, want the branch untouched", f.updates)
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }

// TestADOTokenSourceCredentialSendsTheResolvedValue: the credential resolves
// its value per request and refuses an unknown kind.
func TestADOTokenSourceCredentialSendsTheResolvedValue(t *testing.T) {
	source, err := NewADOTokenSourceCredentialSource(ADOCredentialKindBearer, fixedADOTokenSource("entra-token"))
	if err != nil {
		t.Fatal(err)
	}
	credential, err := source.Credential(context.Background())
	if err != nil || credential.Kind != ADOCredentialKindBearer || credential.Secret != "entra-token" {
		t.Fatalf("credential = %+v, %v", credential, err)
	}
	if _, err := NewADOTokenSourceCredentialSource("digest", fixedADOTokenSource("x")); err == nil {
		t.Fatal("an unknown kind was accepted")
	}
}

type fixedADOTokenSource string

func (s fixedADOTokenSource) Token(context.Context) (string, error) { return string(s), nil }
