//go:build integration

package providers

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// The spec fixture pair `go run ./test/adolive provision` creates (#6125,
// #6191, #6194). These repeat test/adolive/fixtures.go's constants, which a
// package main cannot export; a drift between the two fails these tests loudly
// on the identity check rather than passing on the wrong item.
const (
	adoLiveFixtureTag      = "goobers-live-fixture"
	adoLiveParentTitle     = "goobers live ancestry parent (do not close)"
	adoLiveParentMarker    = "goobers-live ancestry parent intent."
	adoLiveSpecTitle       = "goobers live spec fixture (do not close)"
	adoLiveSpecCriteria    = "goobers-live acceptance criterion: the provider composes this field into the work item body"
	adoLiveSpecWorkItemEnv = "GOOBERS_ADO_LIVE_SPEC_WORK_ITEM"
	adoLiveReadTimeout     = 2 * time.Minute
)

// TestIntegrationADOLiveSmoke is an opt-in, read-only smoke test against a
// real Azure DevOps organization/project (CONF-4, #2077's scheduled ADO
// live leg). Fixture-backed tests (ado_test.go, ado_landing_test.go, the
// test/providers contract corpus) pin behavior against recorded API
// shapes; this exercises the genuine live API surface those fixtures can
// silently drift from.
//
// The //go:build integration tag excludes this from the default `go test
// ./...` and CI's regular gates; it only runs where a caller explicitly
// builds with -tags=integration (the scheduled workflow below, or a
// developer opting in locally). testdep.RequireEnv additionally skips
// (never fails) when the env var isn't set, so any other integration-tagged
// sweep that doesn't provision ADO creds is unaffected.
//
// GOOBERS_ADO_LIVE_REPO is "organization/project/repository". Auth prefers
// a PAT (GOOBERS_ADO_LIVE_TOKEN) since CI runners have no interactive
// Azure CLI session; GOOBERS_ADO_TENANT + an `az login` session is the
// fallback for a developer running this locally without a PAT.
func TestIntegrationADOLiveSmoke(t *testing.T) {
	provider, repo := adoLiveReadSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := provider.RepositoryReachable(ctx, repo); err != nil {
		t.Fatalf("live RepositoryReachable: %v", err)
	}

	items, err := provider.ListWorkItems(ctx, ListWorkItemsRequest{
		Repository: repo, State: "open", Limit: 50,
	})
	if err != nil {
		t.Fatalf("live ListWorkItems: %v", err)
	}
	t.Logf("live smoke ok: %s/%s/%s reachable, %d open work item(s)", repo.Owner, repo.Project, repo.Name, len(items))
}

// TestIntegrationADOLiveSpecFixture reads the provisioned spec fixture: a
// work item with an empty description, acceptance criteria, and a Hierarchy
// parent. It skips until GOOBERS_ADO_LIVE_SPEC_WORK_ITEM names the fixture
// (repository variable ADO_LIVE_SPEC_WORK_ITEM).
func TestIntegrationADOLiveSpecFixture(t *testing.T) {
	provider, repo := adoLiveReadSetup(t)
	testdep.RequireEnv(t, adoLiveSpecWorkItemEnv)
	id := strings.TrimSpace(os.Getenv(adoLiveSpecWorkItemEnv))
	ctx, cancel := context.WithTimeout(context.Background(), adoLiveReadTimeout)
	defer cancel()

	item, err := provider.GetWorkItem(ctx, repo, id)
	if err != nil {
		t.Fatalf("GetWorkItem(%s): %v", id, err)
	}
	if item.Title != adoLiveSpecTitle {
		t.Fatalf("work item %s is %q, not the provisioned spec fixture %q: check ADO_LIVE_SPEC_WORK_ITEM", id, item.Title, adoLiveSpecTitle)
	}
	t.Run("acceptance criteria", func(t *testing.T) { checkADOLiveAcceptanceCriteria(ctx, t, provider, repo, item) })
	t.Run("ancestry", func(t *testing.T) { checkADOLiveAncestry(ctx, t, provider, repo, item) })
}

// checkADOLiveAcceptanceCriteria pins #6191/#6194's inputs on a real item:
// criteria kept only in Boards' Acceptance Criteria field, with an empty
// description, compose into the body, and the listing (workitemsbatch) maps
// the item exactly as the single read does, so a spec digest taken from
// either read matches the other.
func checkADOLiveAcceptanceCriteria(ctx context.Context, t *testing.T, provider *ADOProvider, repo RepositoryRef, item WorkItem) {
	if strings.TrimSpace(item.Description) != "" {
		t.Fatalf("spec fixture description = %q, want empty", item.Description)
	}
	if !strings.Contains(item.AcceptanceCriteria, adoLiveSpecCriteria) {
		t.Fatalf("AcceptanceCriteria = %q, want it to contain %q", item.AcceptanceCriteria, adoLiveSpecCriteria)
	}
	if want := ComposeWorkItemBody(item.Description, item.AcceptanceCriteria); item.Body != want || !strings.HasPrefix(item.Body, "## Acceptance Criteria") {
		t.Fatalf("Body = %q, want the composed %q", item.Body, want)
	}
	listed, err := provider.ListWorkItems(ctx, ListWorkItemsRequest{
		Repository: repo, State: "open", Labels: []string{adoLiveFixtureTag}, OldestFirst: true, Limit: 20,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	i := slices.IndexFunc(listed, func(w WorkItem) bool { return w.ID == item.ID })
	if i < 0 {
		t.Fatalf("ListWorkItems(open, %s) did not return spec fixture %s: is it closed or untagged?", adoLiveFixtureTag, item.ID)
	}
	got := listed[i]
	if got.Body != item.Body || got.AcceptanceCriteria != item.AcceptanceCriteria || got.Description != item.Description || got.Title != item.Title {
		t.Fatalf("list and get disagree on spec fixture %s:\nlist body %q criteria %q\nget  body %q criteria %q",
			item.ID, got.Body, got.AcceptanceCriteria, item.Body, item.AcceptanceCriteria)
	}
}

// checkADOLiveAncestry walks the spec fixture's parents (#6125/#6136): one
// workitemsbatch read reaches the Hierarchy parent, which carries its
// description as an intent field, and the walk ends there, complete.
func checkADOLiveAncestry(ctx context.Context, t *testing.T, provider *ADOProvider, repo RepositoryRef, item WorkItem) {
	root := provider.AncestryRoot(repo, item)
	if root.ParentID == "" {
		t.Fatalf("spec fixture %s has no Hierarchy parent: re-run `go run ./test/adolive provision -apply`", item.ID)
	}
	ancestry, err := TraverseWorkItemAncestry(ctx, provider, repo, []WorkItemNode{root}, AncestryOptions{
		MaxDepth: 3, MaxItems: 5, CrossProject: AncestryCrossProjectDeny, MaxFieldBytes: 4096,
	})
	if err != nil {
		t.Fatalf("TraverseWorkItemAncestry: %v", err)
	}
	if ancestry.Status != AncestryComplete || len(ancestry.Omissions) != 0 {
		t.Fatalf("ancestry status %q, omissions %+v; want complete", ancestry.Status, ancestry.Omissions)
	}
	if len(ancestry.Items) != 1 {
		t.Fatalf("ancestry items = %+v, want exactly the provisioned parent", ancestry.Items)
	}
	parent := ancestry.Items[0]
	if parent.ID != root.ParentID || parent.Title != adoLiveParentTitle || parent.Depth != 1 || !slices.Contains(parent.ParentOf, root.Key()) {
		t.Fatalf("parent = %+v, want #%s %q at depth 1 parenting %s", parent, root.ParentID, adoLiveParentTitle, root.Key())
	}
	if parent.Provider != ProviderADO || strings.TrimSpace(parent.Type) == "" || strings.TrimSpace(parent.State) == "" {
		t.Fatalf("parent identity = %+v, want an ADO node with a type and native state", parent.WorkItemNode)
	}
	i := slices.IndexFunc(parent.Fields, func(f WorkItemField) bool { return f.Name == "System.Description" })
	if i < 0 || !strings.Contains(parent.Fields[i].Value, adoLiveParentMarker) {
		t.Fatalf("parent fields = %+v, want System.Description carrying %q", parent.Fields, adoLiveParentMarker)
	}
	t.Logf("ancestry ok: %s -> #%s (%s, %s)", root.Key(), parent.ID, parent.Type, parent.State)
}

// TestIntegrationADOLiveSignInRejection sends a bearer credential ADO cannot
// accept (#6111). ADO answers a rejected bearer with a redirect to its
// sign-in service rather than a 401; the provider must still take its 401
// path, refreshing the credential once and then failing as an
// authentication error, never as a JSON decode of the sign-in page.
func TestIntegrationADOLiveSignInRejection(t *testing.T) {
	_, repo := adoLiveReadSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), adoLiveReadTimeout)
	defer cancel()

	source := &rotatingADOCredentialSource{token: "goobers-live-rejected-bearer"}
	recorder := &adoLiveStatusRecorder{}
	provider := NewADOProvider(repo.Owner, repo.Project, "",
		WithADOCredentialSource(source),
		WithADOSecretRegistrar(journal.NewRegistryScrubber()),
		func(p *ADOProvider) { p.Client = recorder },
	)
	// A REST read: RepositoryReachable goes through git, not this path.
	_, err := provider.ListPullRequests(ctx, ListPullRequestsRequest{Repository: repo, SkipCheckState: true})
	statuses := recorder.seen()
	t.Logf("ADO answered the rejected bearer with %v; provider error: %v", statuses, err)
	if err == nil {
		t.Fatal("ListPullRequests succeeded with a credential ADO cannot accept")
	}
	if strings.Contains(err.Error(), "invalid character '<'") {
		t.Fatalf("the sign-in page was decoded as JSON instead of read as a 401: %v", err)
	}
	if !IsUnauthorizedError(err) {
		t.Fatalf("error does not carry the 401 the sign-in redirect means: %v", err)
	}
	if !source.invalidated || len(statuses) < 2 {
		t.Fatalf("credential refreshed %v after %d response(s); want the one-shot refresh a 401 triggers", source.invalidated, len(statuses))
	}
	for _, status := range statuses {
		switch status {
		case http.StatusUnauthorized, http.StatusNonAuthoritativeInfo, http.StatusFound:
		default:
			t.Fatalf("ADO answered a rejected bearer with %d; the 401 normalization covers 401, 203 and a sign-in 302 only", status)
		}
	}
}

// adoLiveStatusRecorder records the raw status of each response before the
// provider normalizes it.
type adoLiveStatusRecorder struct {
	mu       sync.Mutex
	statuses []int
}

func (r *adoLiveStatusRecorder) Do(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		r.mu.Lock()
		r.statuses = append(r.statuses, resp.StatusCode)
		r.mu.Unlock()
	}
	return resp, err
}

func (r *adoLiveStatusRecorder) seen() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.statuses)
}

// adoLiveReadSetup builds the read-only leg's provider from
// GOOBERS_ADO_LIVE_REPO (organization/project/repository), skipping when it
// is unset.
func adoLiveReadSetup(t *testing.T) (*ADOProvider, RepositoryRef) {
	t.Helper()
	testdep.RequireEnv(t, "GOOBERS_ADO_LIVE_REPO")
	target := os.Getenv("GOOBERS_ADO_LIVE_REPO")
	parts := strings.Split(target, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		t.Fatalf("GOOBERS_ADO_LIVE_REPO = %q, want organization/project/repository", target)
	}
	repo := RepositoryRef{Provider: ProviderADO, Owner: parts[0], Project: parts[1], Name: parts[2]}
	if token := os.Getenv("GOOBERS_ADO_LIVE_TOKEN"); token != "" {
		return NewADOProvider(parts[0], parts[1], token,
			WithADOSecretRegistrar(journal.NewRegistryScrubber())), repo
	}
	source := NewAzureCLIADOCredentialSource(nil, os.Getenv("GOOBERS_ADO_TENANT"))
	return NewADOProvider(parts[0], parts[1], "",
		WithADOCredentialSource(source),
		WithADOSecretRegistrar(journal.NewRegistryScrubber()),
	), repo
}
