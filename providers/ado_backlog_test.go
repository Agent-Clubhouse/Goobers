package providers

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
)

// TestADOMergeLabels pins the add/remove label reconciliation UpdateWorkItem
// relies on: removed labels drop, added labels append, and the result is
// de-duplicated with order preserved.
func TestADOMergeLabels(t *testing.T) {
	got := applyLabelSet(
		[]string{"route/backend", "goobers/status:claimed", "keep"},
		[]string{"goobers/status:in-progress", "keep"},
		[]string{"goobers/status:claimed"},
	)
	want := []string{"route/backend", "keep", "goobers/status:in-progress"}
	if len(got) != len(want) {
		t.Fatalf("applyLabelSet = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("applyLabelSet[%d] = %q, want %q (full %#v)", i, got[i], want[i], got)
		}
	}
}

// TestADOListWorkItemsFiltersByTagsInWIQL pins the server-side tag filter: the
// backlog label(s) must land in the WIQL as [System.Tags] CONTAINS predicates.
// Without them a large project returns every open item and ADO 400s past its
// 20000-row cap before any client-side label filtering runs.
func TestADOListWorkItemsFiltersByTagsInWIQL(t *testing.T) {
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode WIQL: %v", err)
		}
		gotQuery = body.Query
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}
	if _, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{Repository: repo, State: "open", Labels: []string{"example-label"}}); err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if !strings.Contains(gotQuery, "[System.Tags] CONTAINS 'example-label'") {
		t.Fatalf("query = %q, want it to filter by [System.Tags] CONTAINS 'example-label'", gotQuery)
	}
}

func TestADOClaimFailsWhenWrittenBreadcrumbIsNotVisible(t *testing.T) {
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	handleADOTestConnectionData(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/workItems/42/comments", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{"comments": []interface{}{}})
		case http.MethodPost:
			writeJSON(t, w, map[string]interface{}{"commentId": 1, "text": "accepted but not persisted"})
		default:
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{
			"id": 42, "rev": 1,
			"fields": map[string]interface{}{
				"System.WorkItemType": "Issue",
				"System.State":        "New",
				"System.Title":        "Claim candidate",
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	_, err := provider.ClaimWorkItem(context.Background(), ClaimWorkItemRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		ID:         "42",
		RunID:      "run-42",
	})
	if err == nil || !strings.Contains(err.Error(), "not visible after write") {
		t.Fatalf("ClaimWorkItem error = %v, want missing-breadcrumb failure", err)
	}
}

// adoTestSelfID is the identity GUID the ADO claim fakes report from
// connectionData; adoTestOtherID is a different project member.
const (
	adoTestSelfID  = "00000000-0000-0000-0000-0000000005e1"
	adoTestOtherID = "00000000-0000-0000-0000-00000000077e"
)

func handleADOTestConnectionData(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	mux.HandleFunc("/org/_apis/connectionData", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"authenticatedUser": map[string]interface{}{
			"id": adoTestSelfID, "providerDisplayName": "Goobers Bot",
		}})
	})
}

// adoClaimFake is an in-memory work item 42 with a comment thread. Comments the
// provider posts are stamped with adoTestSelfID; seeded comments carry
// whatever author the test gives them.
//
// Tag writes behave like ADO's shared, case-insensitive tag namespace: a tag
// already present keeps the casing it was first written with.
type adoClaimFake struct {
	mu          sync.Mutex
	comments    []map[string]interface{}
	tags        string
	patchedTags []string
}

// adoTestFirstWriterTags applies a System.Tags write the way ADO does: the
// written set replaces the old one, but a tag matching an existing one
// ignoring case keeps the existing casing.
func adoTestFirstWriterTags(existing, written string) string {
	old := adoLabels(existing)
	var out []string
	for _, tag := range adoLabels(written) {
		for _, have := range old {
			if strings.EqualFold(have, tag) {
				tag = have
				break
			}
		}
		if !adoHasLabel(out, tag) {
			out = append(out, tag)
		}
	}
	return strings.Join(out, "; ")
}

func (f *adoClaimFake) seed(authorID, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, map[string]interface{}{
		"commentId": len(f.comments) + 1,
		"text":      text,
		// A forger can pick any display name; only the id identifies them.
		"createdBy": map[string]string{"id": authorID, "displayName": "Goobers Bot"},
	})
}

func (f *adoClaimFake) server(t *testing.T, identity bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	if identity {
		handleADOTestConnectionData(t, mux)
	} else {
		mux.HandleFunc("/org/_apis/connectionData", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusUnauthorized)
		})
	}
	mux.HandleFunc("/org/project/_apis/wit/workItems/42/comments", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			comments := append([]map[string]interface{}(nil), f.comments...)
			f.mu.Unlock()
			writeJSON(t, w, map[string]interface{}{"comments": comments})
		case http.MethodPost:
			var body map[string]string
			decodeJSON(t, r, &body)
			f.seed(adoTestSelfID, body["text"])
			writeJSON(t, w, map[string]interface{}{"commentId": 99, "text": body["text"]})
		default:
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodPatch {
			var ops []map[string]interface{}
			decodeJSON(t, r, &ops)
			for _, op := range ops {
				if op["path"] == "/fields/System.Tags" {
					value, _ := op["value"].(string)
					f.patchedTags = append(f.patchedTags, value)
					f.tags = adoTestFirstWriterTags(f.tags, value)
				}
			}
		}
		writeJSON(t, w, map[string]interface{}{
			"id": 42, "rev": 1,
			"fields": map[string]interface{}{
				"System.WorkItemType": "Issue",
				"System.State":        "New",
				"System.Title":        "Claim candidate",
				"System.Tags":         f.tags,
			},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func claimADOTestItem(t *testing.T, server *httptest.Server, runID string) (ClaimResult, error) {
	t.Helper()
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	return provider.ClaimWorkItem(context.Background(), ClaimWorkItemRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		ID:         "42",
		RunID:      runID,
	})
}

// TestADOClaimIgnoresBreadcrumbFromAnotherIdentity pins ADO-N10: a claim
// breadcrumb posted by a different identity — even one sharing the bot's
// display name and posted first — never wins the claim.
func TestADOClaimIgnoresBreadcrumbFromAnotherIdentity(t *testing.T) {
	fake := &adoClaimFake{}
	fake.seed(adoTestOtherID, claimBreadcrumb("run-forged"))
	result, err := claimADOTestItem(t, fake.server(t, true), "run-ours")
	if err != nil {
		t.Fatalf("ClaimWorkItem: %v", err)
	}
	if !result.Claimed || result.ClaimedBy != "run-ours" {
		t.Fatalf("claim = %#v, want run-ours to win over a forged breadcrumb", result)
	}
}

// TestADOClaimIgnoresReleaseFromAnotherIdentity pins ADO-N10: a release
// breadcrumb from another identity cannot end the real owner's claim epoch.
func TestADOClaimIgnoresReleaseFromAnotherIdentity(t *testing.T) {
	fake := &adoClaimFake{}
	fake.seed(adoTestSelfID, claimBreadcrumb("run-owner"))
	fake.seed(adoTestOtherID, claimReleaseBreadcrumb("run-owner"))
	result, err := claimADOTestItem(t, fake.server(t, true), "run-ours")
	if err != nil {
		t.Fatalf("ClaimWorkItem: %v", err)
	}
	if result.Claimed || result.ClaimedBy != "run-owner" {
		t.Fatalf("claim = %#v, want run-owner to keep the claim despite a forged release", result)
	}
}

// TestADOClaimFailsClosedWithoutIdentity pins ADO-N10: when the authenticated
// identity cannot be read, the claim errors instead of scanning breadcrumbs
// unfiltered, and nothing is written.
func TestADOClaimFailsClosedWithoutIdentity(t *testing.T) {
	fake := &adoClaimFake{}
	_, err := claimADOTestItem(t, fake.server(t, false), "run-ours")
	if err == nil || !strings.Contains(err.Error(), "resolve claim marker author") {
		t.Fatalf("ClaimWorkItem error = %v, want identity resolution failure", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.comments) != 0 {
		t.Fatalf("claim wrote %d comment(s) without a resolved identity", len(fake.comments))
	}
}

// TestADOReleaseWorkItemClaimRetiresEpochAndVisibleMarker pins ADO-N29:
// releasing an ADO claim posts a release breadcrumb and clears the visible
// claim label, ending the epoch so a later claimant is not stuck behind it.
func TestADOReleaseWorkItemClaimRetiresEpochAndVisibleMarker(t *testing.T) {
	fake := &adoClaimFake{tags: "goobers:approved; " + LabelClaimed}
	fake.seed(adoTestSelfID, claimBreadcrumb("run-42"))
	server := fake.server(t, true)

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Provider: ProviderADO, Name: "repo", Project: "project"}
	released, err := provider.ReleaseWorkItemClaim(context.Background(), ClaimWorkItemRequest{
		Repository: repo,
		ID:         "42",
		RunID:      "run-42",
	})
	if err != nil {
		t.Fatalf("ReleaseWorkItemClaim: %v", err)
	}
	fake.mu.Lock()
	tags := fake.tags
	fake.mu.Unlock()
	if released.HasLabel(LabelClaimed) || strings.Contains(tags, LabelClaimed) {
		t.Fatalf("released ADO item still has %q: item=%v raw tags=%q", LabelClaimed, released.Labels, tags)
	}
	winner, claimed, err := provider.adoClaimWinner(context.Background(), repo, "42")
	if err != nil {
		t.Fatalf("adoClaimWinner after release: %v", err)
	}
	if claimed || winner != "" {
		t.Fatalf("ADO claim winner after release = (%q, %v), want no active epoch", winner, claimed)
	}
}

// TestADOReleaseWorkItemClaimPreservesNewerOwner pins ADO-N29: a stale
// terminal-cleanup release for a run that no longer owns the item must not
// clobber a newer claimant's ownership, and must not write anything while
// refusing.
func TestADOReleaseWorkItemClaimPreservesNewerOwner(t *testing.T) {
	comments := []map[string]interface{}{
		{"commentId": 1, "text": claimBreadcrumb("old-run"), "createdBy": map[string]string{"id": adoTestSelfID}},
		{"commentId": 2, "text": claimReleaseBreadcrumb("old-run"), "createdBy": map[string]string{"id": adoTestSelfID}},
		{"commentId": 3, "text": claimBreadcrumb("new-run"), "createdBy": map[string]string{"id": adoTestSelfID}},
	}
	var mutations int

	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	handleADOTestConnectionData(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/workItems/42/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations++
		}
		writeJSON(t, w, map[string]interface{}{"comments": comments})
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations++
		}
		writeJSON(t, w, map[string]interface{}{
			"id": 42, "rev": 3,
			"fields": map[string]interface{}{
				"System.WorkItemType": "Issue",
				"System.Title":        "Reclaimed ADO item",
				"System.State":        "Active",
				"System.Tags":         LabelClaimed,
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	_, err := provider.ReleaseWorkItemClaim(context.Background(), ClaimWorkItemRequest{
		Repository: RepositoryRef{Provider: ProviderADO, Name: "repo", Project: "project"},
		ID:         "42",
		RunID:      "old-run",
	})
	if err == nil || !strings.Contains(err.Error(), `held by run "new-run"`) {
		t.Fatalf("ReleaseWorkItemClaim error = %v, want newer-owner refusal", err)
	}
	if mutations != 0 {
		t.Fatalf("newer-owner refusal performed %d provider mutation(s), want none", mutations)
	}
}

// TestADOClaimIgnoresLegacyOwnerTag pins ADO-N38: a stray
// goobers:claim-run:<b64> tag left on an item from before the claim-tag
// fallback was removed no longer confers a claim when there is no breadcrumb
// backing it.
func TestADOClaimIgnoresLegacyOwnerTag(t *testing.T) {
	fake := &adoClaimFake{tags: "goobers:claim-run:cnVuLWxlZ2FjeQ"}
	result, err := claimADOTestItem(t, fake.server(t, true), "run-ours")
	if err != nil {
		t.Fatalf("ClaimWorkItem: %v", err)
	}
	if !result.Claimed || result.ClaimedBy != "run-ours" {
		t.Fatalf("claim = %#v, want run-ours to win over a stray legacy tag", result)
	}
}

// TestADOFindPullRequestByBranch pins the exact source-branch match the
// idempotent OpenPullRequest and issue-close-out linking rely on: a prefix
// collision ("run-1" vs "run-10") must not resolve the wrong PR.
func TestADOFindPullRequestByBranch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"value": []map[string]interface{}{
			{"pullRequestId": 10, "url": "pr-10", "sourceRefName": "refs/heads/run-10", "targetRefName": "refs/heads/main"},
			{"pullRequestId": 1, "url": "pr-1", "sourceRefName": "refs/heads/run-1", "targetRefName": "refs/heads/main"},
		}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}

	pr, found, err := provider.FindPullRequestByBranch(context.Background(), repo, "run-1", "main")
	if err != nil {
		t.Fatalf("FindPullRequestByBranch: %v", err)
	}
	if !found || pr.Number != 1 {
		t.Fatalf("FindPullRequestByBranch(run-1) = %#v found=%v, want PR 1", pr, found)
	}

	if _, found, err := provider.FindPullRequestByBranch(context.Background(), repo, "run-999", "main"); err != nil || found {
		t.Fatalf("FindPullRequestByBranch(run-999) found=%v err=%v, want not found", found, err)
	}
}

func TestADODecompositionMarkerAndCommentMutations(t *testing.T) {
	const marker = "<!-- goobers-action:v1 key=child -->"
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$top"); got != strconv.Itoa(adoWIQLPageSize) {
			t.Fatalf("$top = %q, want %d", got, adoWIQLPageSize)
		}
		var body struct {
			Query string `json:"query"`
		}
		decodeJSON(t, r, &body)
		if strings.Contains(body.Query, "Description") {
			t.Fatalf("query = %q, must not use full-text description search", body.Query)
		}
		writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 1}, {"id": 2}}})
	})
	for id, description := range map[int]string{1: "body\n" + marker, 2: "prefix " + marker} {
		mux.HandleFunc("/org/project/_apis/wit/workitems/"+strconv.Itoa(id), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]interface{}{
				"id": id, "rev": 1,
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue",
					"System.State":        "New",
					"System.Description":  description,
				},
			})
		})
	}
	mux.HandleFunc("/org/project/_apis/wit/workItems/7/comments", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		if got := r.URL.Query().Get("format"); got != "markdown" {
			t.Fatalf("comment format = %q, want markdown (ADO-N27)", got)
		}
		var body map[string]string
		decodeJSON(t, r, &body)
		writeJSON(t, w, map[string]interface{}{"commentId": 9, "text": body["text"]})
	})
	server := httptest.NewServer(withADOTestWorkItemsBatch(t, mux))
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	repo := RepositoryRef{Name: "repo", Project: "project"}
	items, err := provider.FindWorkItemsByMarker(context.Background(), repo, marker)
	if err != nil {
		t.Fatalf("FindWorkItemsByMarker: %v", err)
	}
	if len(items) != 1 || items[0].ID != "1" {
		t.Fatalf("items = %#v, want exact marker match #1", items)
	}
	comment, err := provider.CreateWorkItemComment(context.Background(), repo, "7", "prepared")
	if err != nil {
		t.Fatalf("CreateWorkItemComment: %v", err)
	}
	if comment.ID != "9" || comment.Body != "prepared" {
		t.Fatalf("comment = %#v", comment)
	}
}

// TestADOCreateWorkItemDefaultTypeFromRequirementCategory pins ADO-N27: with
// no req.Type, CreateWorkItem resolves the project's create type from the
// Requirement category's default work item type rather than a hard-coded
// "Issue", so it works on every stock process and on a custom process that
// renamed the type while keeping the category's own referenceName stable.
// Each case also proves state resolution (adoWorkItemStateCategories, used
// afterwards to map the created item's state) keys on the type's name, not
// its referenceName — states are read at workitemtypes/{name}/states, which
// only works if the type name, not an internal referenceName, is what gets
// sent.
func TestADOCreateWorkItemDefaultTypeFromRequirementCategory(t *testing.T) {
	for _, tc := range []struct {
		name        string
		process     string
		defaultType string
	}{
		{name: "basic", process: "Basic", defaultType: "Issue"},
		{name: "agile", process: "Agile", defaultType: "User Story"},
		{name: "scrum", process: "Scrum", defaultType: "Product Backlog Item"},
		{
			name:        "inherited process renames the type",
			process:     "Inherited from Agile",
			defaultType: "Example Requirement",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var categoryCalls, createCalls int
			mux := http.NewServeMux()
			mux.HandleFunc("/org/project/_apis/wit/workitemtypecategories/Microsoft.RequirementCategory", func(w http.ResponseWriter, r *http.Request) {
				assertMethod(t, r, http.MethodGet)
				categoryCalls++
				writeJSON(t, w, map[string]interface{}{
					"name":                "Requirement Category",
					"referenceName":       "Microsoft.RequirementCategory",
					"defaultWorkItemType": map[string]string{"name": tc.defaultType},
				})
			})
			// Registered as path prefixes rather than exact literal patterns:
			// Go 1.22's ServeMux reads a bare space in a pattern as the start
			// of a method verb, which the default type's real-world names
			// ("User Story", "Product Backlog Item") contain.
			mux.HandleFunc("/org/project/_apis/wit/workitemtypes/", func(w http.ResponseWriter, r *http.Request) {
				assertMethod(t, r, http.MethodGet)
				if !strings.HasSuffix(r.URL.Path, "/states") || r.URL.Path != "/org/project/_apis/wit/workitemtypes/"+tc.defaultType+"/states" {
					t.Fatalf("states path = %q, want type %q", r.URL.Path, tc.defaultType)
				}
				writeJSON(t, w, map[string]interface{}{"value": []map[string]string{
					{"name": "New", "category": "Proposed"},
				}})
			})
			mux.HandleFunc("/org/project/_apis/wit/workitems/", func(w http.ResponseWriter, r *http.Request) {
				assertMethod(t, r, http.MethodPost)
				if r.URL.Path != "/org/project/_apis/wit/workitems/$"+tc.defaultType {
					t.Fatalf("create path = %q, want type %q", r.URL.Path, tc.defaultType)
				}
				createCalls++
				var patch []adoPatchOperation
				decodeJSON(t, r, &patch)
				var hasMarkdownOp bool
				for _, op := range patch {
					if op.Path == "/multilineFieldsFormat/System.Description" && op.Value == "Markdown" {
						hasMarkdownOp = true
					}
				}
				if !hasMarkdownOp {
					t.Fatalf("create patch missing multilineFieldsFormat op: %#v", patch)
				}
				writeJSON(t, w, map[string]interface{}{
					"id": 61, "rev": 1, "url": "item-url",
					"fields": map[string]interface{}{
						"System.WorkItemType": tc.defaultType,
						"System.Title":        "New work",
						"System.State":        "New",
					},
				})
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			repo := RepositoryRef{Name: "repo", Project: "project"}
			item, err := provider.CreateWorkItem(context.Background(), CreateWorkItemRequest{
				Repository: repo,
				Title:      "New work",
				Body:       "details",
			})
			if err != nil {
				t.Fatalf("CreateWorkItem (%s process): %v", tc.process, err)
			}
			if item.Type != tc.defaultType {
				t.Fatalf("created item type = %q, want %q", item.Type, tc.defaultType)
			}
			if createCalls != 1 {
				t.Fatalf("create POSTs to workitems/$%s = %d, want 1", tc.defaultType, createCalls)
			}

			// A second create against a different run reuses the cached
			// default type: no repeat GET against workitemtypecategories.
			if _, err := provider.CreateWorkItem(context.Background(), CreateWorkItemRequest{
				Repository: repo, Title: "New work 2", Body: "more details",
			}); err != nil {
				t.Fatalf("second CreateWorkItem: %v", err)
			}
			if categoryCalls != 1 {
				t.Fatalf("workitemtypecategories calls = %d, want 1 (type cache not hit)", categoryCalls)
			}
			if createCalls != 2 {
				t.Fatalf("create POSTs = %d, want 2", createCalls)
			}
		})
	}
}

func TestADOFindWorkItemsByMarkerPagesByID(t *testing.T) {
	const marker = "<!-- goobers-action:v1 key=second-page -->"
	var queries []string
	mux := http.NewServeMux()
	handleADOTestStateCategories(t, mux)
	mux.HandleFunc("/org/project/_apis/wit/wiql", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("$top"); got != "2" {
			t.Fatalf("$top = %q, want 2", got)
		}
		var body struct {
			Query string `json:"query"`
		}
		decodeJSON(t, r, &body)
		queries = append(queries, body.Query)
		switch {
		case strings.Contains(body.Query, "[System.Id] > 2"):
			writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 3}}})
		case strings.Contains(body.Query, "[System.Id] >"):
			t.Fatalf("unexpected cursor query %q", body.Query)
		default:
			writeJSON(t, w, map[string]interface{}{"workItems": []map[string]int{{"id": 1}, {"id": 2}}})
		}
	})
	for id := 1; id <= 3; id++ {
		description := "body"
		if id == 3 {
			description += "\n" + marker
		}
		mux.HandleFunc("/org/project/_apis/wit/workitems/"+strconv.Itoa(id), func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]interface{}{
				"id": id, "rev": 1,
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue",
					"System.State":        "New",
					"System.Description":  description,
				},
			})
		})
	}
	server := httptest.NewServer(withADOTestWorkItemsBatch(t, mux))
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	items, err := provider.findWorkItemsByMarker(
		context.Background(),
		RepositoryRef{Name: "repo", Project: "project"},
		marker,
		2,
	)
	if err != nil {
		t.Fatalf("findWorkItemsByMarker: %v", err)
	}
	if len(items) != 1 || items[0].ID != "3" {
		t.Fatalf("items = %#v, want exact marker match #3", items)
	}
	if len(queries) != 2 {
		t.Fatalf("queries = %#v, want two pages", queries)
	}
	if !strings.HasSuffix(queries[0], "ORDER BY [System.Id] ASC") {
		t.Fatalf("first query = %q, want deterministic ID order", queries[0])
	}
	if !strings.HasSuffix(queries[1], "AND [System.Id] > 2 ORDER BY [System.Id] ASC") {
		t.Fatalf("second query = %q, want ID cursor after first page", queries[1])
	}
}

// adoTestStates registers a per-type work-item state table under
// /org/project/_apis/wit/workitemtypes/<type>/states, so a test can give
// different types different Completed/Resolved shapes.
func adoTestStates(t *testing.T, mux *http.ServeMux, byType map[string][]map[string]string) {
	t.Helper()
	mux.HandleFunc("/org/project/_apis/wit/workitemtypes/", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		itemType := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/org/project/_apis/wit/workitemtypes/"), "/states")
		states, ok := byType[itemType]
		if !ok {
			t.Fatalf("unexpected work item type in states request: %q", itemType)
		}
		writeJSON(t, w, map[string]interface{}{"value": states})
	})
}

// TestADOCloseWorkItemResolvedAdvancesToCompleted pins the middle step: a
// Bug sitting in Resolved (as transitionWorkItems or a "Fixes #" commit link
// would leave it) advances to the type's Completed state.
func TestADOCloseWorkItemResolvedAdvancesToCompleted(t *testing.T) {
	var patchBody []adoPatchOperation
	mux := http.NewServeMux()
	adoTestStates(t, mux, map[string][]map[string]string{
		"Bug": {
			{"name": "New", "category": "Proposed"},
			{"name": "Active", "category": "InProgress"},
			{"name": "Resolved", "category": "Resolved"},
			{"name": "Closed", "category": "Completed"},
		},
	})
	mux.HandleFunc("/org/project/_apis/wit/workitems/7", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{
				"id": 7, "rev": 2, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Bug", "System.Title": "Crash",
					"System.State": "Resolved", "System.Tags": "goobers/status:in-progress",
				},
			})
		case http.MethodPatch:
			decodeJSON(t, r, &patchBody)
			writeJSON(t, w, map[string]interface{}{
				"id": 7, "rev": 3, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Bug", "System.Title": "Crash",
					"System.State": "Closed", "System.Tags": "goobers/status:done",
				},
			})
		default:
			t.Fatalf("unexpected work item method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	item, err := provider.UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"}, ID: "7", Status: WorkItemStatusDone,
	})
	if err != nil {
		t.Fatalf("UpdateWorkItemStatus: %v", err)
	}
	if item.State != "closed" {
		t.Fatalf("state = %q, want closed", item.State)
	}
	found := false
	for _, op := range patchBody {
		if op.Path == "/fields/System.State" && op.Value == "Closed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("patch body = %#v, want a System.State=Closed op", patchBody)
	}
}

// TestADOCloseWorkItemRetriesRevisionConflict pins the 412 handling: a
// revision conflict on the first close PATCH (attempting the System.State
// change) re-reads and retries; the re-read shows the item already Done —
// closed by a racing external write — so the retried PATCH carries no
// System.State op and succeeds.
func TestADOCloseWorkItemRetriesRevisionConflict(t *testing.T) {
	var stateOpsByAttempt []int
	mux := http.NewServeMux()
	adoTestStates(t, mux, map[string][]map[string]string{
		"Issue": {
			{"name": "New", "category": "Proposed"},
			{"name": "Active", "category": "InProgress"},
			{"name": "Resolved", "category": "Resolved"},
			{"name": "Done", "category": "Completed"},
		},
	})
	var gets, patches int
	mux.HandleFunc("/org/project/_apis/wit/workitems/11", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			gets++
			state := "Active"
			if gets > 1 {
				// The first PATCH's conflict means someone else closed it.
				state = "Done"
			}
			writeJSON(t, w, map[string]interface{}{
				"id": 11, "rev": gets, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue", "System.Title": "Race",
					"System.State": state, "System.Tags": "goobers/status:claimed",
				},
			})
		case http.MethodPatch:
			patches++
			var body []adoPatchOperation
			decodeJSON(t, r, &body)
			stateOps := 0
			for _, op := range body {
				if op.Path == "/fields/System.State" {
					stateOps++
				}
			}
			stateOpsByAttempt = append(stateOpsByAttempt, stateOps)
			if patches == 1 {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = w.Write([]byte(`{"message":"rev mismatch"}`))
				return
			}
			writeJSON(t, w, map[string]interface{}{
				"id": 11, "rev": gets + 1, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue", "System.Title": "Race",
					"System.State": "Done", "System.Tags": "goobers/status:done",
				},
			})
		default:
			t.Fatalf("unexpected work item method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	item, err := provider.UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"}, ID: "11", Status: WorkItemStatusDone,
	})
	if err != nil {
		t.Fatalf("UpdateWorkItemStatus: %v", err)
	}
	if item.State != "closed" {
		t.Fatalf("state = %q, want closed", item.State)
	}
	if patches != 2 {
		t.Fatalf("patch attempts = %d, want exactly two (conflict, then a clean retry)", patches)
	}
	if len(stateOpsByAttempt) != 2 || stateOpsByAttempt[0] != 1 || stateOpsByAttempt[1] != 0 {
		t.Fatalf("System.State ops per attempt = %#v, want [1 0]: the retry finds it already Done", stateOpsByAttempt)
	}
}

// TestADOCloseWorkItemBoundedOnRepeatedConflict pins the failure mode: a
// close that conflicts on every retry attempt returns a bounded error
// instead of looping forever.
func TestADOCloseWorkItemBoundedOnRepeatedConflict(t *testing.T) {
	mux := http.NewServeMux()
	adoTestStates(t, mux, map[string][]map[string]string{
		"Issue": {
			{"name": "New", "category": "Proposed"},
			{"name": "Active", "category": "InProgress"},
			{"name": "Resolved", "category": "Resolved"},
			{"name": "Done", "category": "Completed"},
		},
	})
	var patches int
	mux.HandleFunc("/org/project/_apis/wit/workitems/13", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, map[string]interface{}{
				"id": 13, "rev": 1, "url": "item-url",
				"fields": map[string]interface{}{
					"System.WorkItemType": "Issue", "System.Title": "Stuck",
					"System.State": "Active", "System.Tags": "goobers/status:claimed",
				},
			})
		case http.MethodPatch:
			patches++
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(`{"message":"rev mismatch"}`))
		default:
			t.Fatalf("unexpected work item method %s", r.Method)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	_, err := provider.UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"}, ID: "13", Status: WorkItemStatusDone,
	})
	if err == nil {
		t.Fatalf("UpdateWorkItemStatus: want a bounded error, got success")
	}
	if patches != adoClaimRetries {
		t.Fatalf("patch attempts = %d, want %d (bounded)", patches, adoClaimRetries)
	}
}

// adoCloseFake serves one work item, its type's state table, and its
// comment thread for the close tests. reads[i] is the (state, tags) the
// i-th GET returns (the last entry repeats) and the i-th read carries rev
// i+1; patchStatus[i] is the HTTP status of the i-th PATCH (missing or 0 =
// success, echoing the patched state and tags).
type adoCloseFake struct {
	t           *testing.T
	id          string
	itemType    string
	states      []map[string]string
	reads       [][2]string
	patchStatus []int
	patchBody   string
	commentFail bool

	mu       sync.Mutex
	gets     int
	patches  [][]adoPatchOperation
	comments []string
}

func (f *adoCloseFake) server() *httptest.Server {
	mux := http.NewServeMux()
	adoTestStates(f.t, mux, map[string][]map[string]string{f.itemType: f.states})
	mux.HandleFunc("/org/project/_apis/wit/workitems/"+f.id, f.serveItem)
	mux.HandleFunc("/org/project/_apis/wit/workItems/"+f.id+"/comments", f.serveComments)
	return httptest.NewServer(mux)
}

func (f *adoCloseFake) item(rev int, state, tags string) map[string]interface{} {
	id, _ := strconv.Atoi(f.id)
	return map[string]interface{}{
		"id": id, "rev": rev, "url": "item-url",
		"fields": map[string]interface{}{
			"System.WorkItemType": f.itemType, "System.Title": "Item",
			"System.State": state, "System.Tags": tags,
		},
	}
}

func (f *adoCloseFake) serveItem(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		read := f.reads[min(f.gets, len(f.reads)-1)]
		f.gets++
		writeJSON(f.t, w, f.item(f.gets, read[0], read[1]))
	case http.MethodPatch:
		var body []adoPatchOperation
		decodeJSON(f.t, r, &body)
		f.patches = append(f.patches, body)
		if n := len(f.patches) - 1; n < len(f.patchStatus) && f.patchStatus[n] != 0 {
			w.WriteHeader(f.patchStatus[n])
			_, _ = w.Write([]byte(f.patchBody))
			return
		}
		read := f.reads[min(f.gets, len(f.reads))-1]
		state, tags := read[0], read[1]
		if v, ok := adoPatchValue(body, "/fields/System.State"); ok {
			state = v
		}
		if v, ok := adoPatchValue(body, "/fields/System.Tags"); ok {
			tags = v
		}
		writeJSON(f.t, w, f.item(f.gets+1, state, tags))
	default:
		f.t.Errorf("unexpected work item method %s", r.Method)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *adoCloseFake) serveComments(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		page := make([]map[string]interface{}, 0, len(f.comments))
		for i, text := range f.comments {
			page = append(page, map[string]interface{}{"id": i + 1, "text": text})
		}
		writeJSON(f.t, w, map[string]interface{}{"comments": page})
	case http.MethodPost:
		if f.commentFail {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"comment rejected"}`))
			return
		}
		var body struct {
			Text string `json:"text"`
		}
		decodeJSON(f.t, r, &body)
		f.comments = append(f.comments, body.Text)
		writeJSON(f.t, w, map[string]interface{}{"id": len(f.comments), "text": body.Text})
	default:
		f.t.Errorf("unexpected comments method %s", r.Method)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// adoPatchValue returns the string value of the op at path, if present.
func adoPatchValue(ops []adoPatchOperation, path string) (string, bool) {
	for _, op := range ops {
		if op.Path == path {
			value, _ := op.Value.(string)
			return value, true
		}
	}
	return "", false
}

var adoTestIssueStates = []map[string]string{
	{"name": "New", "category": "Proposed"},
	{"name": "Active", "category": "InProgress"},
	{"name": "Resolved", "category": "Resolved"},
	{"name": "Done", "category": "Completed"},
	{"name": "Removed", "category": "Removed"},
}

func newADOCloseTestProvider(server *httptest.Server) *ADOProvider {
	return NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
}

var adoCloseTestRepo = RepositoryRef{Name: "repo", Project: "project"}

// TestADOCloseWorkItemAlreadyClosedAfterConflictIsNoOp pins the
// idempotent-close short circuit: the close PATCH conflicts because the
// server moved the item on its own, the re-read shows a Removed-category
// state, and the retry sends only the status tag with no System.State op.
func TestADOCloseWorkItemAlreadyClosedAfterConflictIsNoOp(t *testing.T) {
	fake := &adoCloseFake{
		t: t, id: "42", itemType: "Issue", states: adoTestIssueStates,
		reads: [][2]string{
			{"Active", "goobers/status:claimed"},
			{"Removed", "goobers/status:claimed"},
		},
		patchStatus: []int{http.StatusPreconditionFailed},
		patchBody:   `{"message":"rev mismatch"}`,
	}
	server := fake.server()
	defer server.Close()

	item, err := newADOCloseTestProvider(server).UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: adoCloseTestRepo, ID: "42", Status: WorkItemStatusDone,
	})
	if err != nil {
		t.Fatalf("UpdateWorkItemStatus: %v", err)
	}
	if item.State != "closed" {
		t.Fatalf("state = %q, want closed", item.State)
	}
	if len(fake.patches) != 2 {
		t.Fatalf("patches = %d, want 2 (conflict, then retry)", len(fake.patches))
	}
	if _, ok := adoPatchValue(fake.patches[1], "/fields/System.State"); ok {
		t.Fatalf("retry patch = %#v, want no System.State op for an already-Removed item", fake.patches[1])
	}
	if tags, _ := adoPatchValue(fake.patches[1], "/fields/System.Tags"); tags != "goobers/status:done" {
		t.Fatalf("retry tags = %q, want the done status tag", tags)
	}
}

// TestADOUpdateWorkItemCloseRetriesWithRecomputedFields pins the
// UpdateWorkItem(State: closed) path through the close loop: the first
// PATCH (title, labels, state) conflicts, the re-read shows the item Done
// with a tag someone else added, and the retry carries the title and tags
// recomputed from that re-read but no System.State op.
func TestADOUpdateWorkItemCloseRetriesWithRecomputedFields(t *testing.T) {
	fake := &adoCloseFake{
		t: t, id: "21", itemType: "Issue", states: adoTestIssueStates,
		reads: [][2]string{
			{"Active", "goobers/status:claimed"},
			{"Done", "goobers/status:claimed; external"},
		},
		patchStatus: []int{http.StatusPreconditionFailed},
		patchBody:   `{"message":"rev mismatch"}`,
	}
	server := fake.server()
	defer server.Close()

	title := "Renamed"
	item, err := newADOCloseTestProvider(server).UpdateWorkItem(context.Background(), UpdateWorkItemRequest{
		Repository: adoCloseTestRepo, ID: "21", State: "closed", Title: &title,
		AddLabels: []string{"shipped"}, RemoveLabels: []string{"goobers/status:claimed"},
	})
	if err != nil {
		t.Fatalf("UpdateWorkItem: %v", err)
	}
	if item.State != "closed" {
		t.Fatalf("state = %q, want closed", item.State)
	}
	if fake.gets != 2 {
		t.Fatalf("item reads = %d, want 2 (the caller's read reused, then one re-read)", fake.gets)
	}
	if len(fake.patches) != 2 {
		t.Fatalf("patches = %d, want 2 (conflict, then retry)", len(fake.patches))
	}
	if state, ok := adoPatchValue(fake.patches[0], "/fields/System.State"); !ok || state != "Done" {
		t.Fatalf("first patch = %#v, want System.State=Done", fake.patches[0])
	}
	retry := fake.patches[1]
	if _, ok := adoPatchValue(retry, "/fields/System.State"); ok {
		t.Fatalf("retry patch = %#v, want no System.State op once the re-read shows Done", retry)
	}
	if got, _ := adoPatchValue(retry, "/fields/System.Title"); got != title {
		t.Fatalf("retry title = %q, want %q", got, title)
	}
	if tags, _ := adoPatchValue(retry, "/fields/System.Tags"); tags != "external; shipped" {
		t.Fatalf("retry tags = %q, want tags recomputed from the re-read", tags)
	}
	if rev := retry[0]; rev.Path != "/rev" || rev.Value != float64(2) {
		t.Fatalf("retry guard = %#v, want test /rev against the re-read rev 2", rev)
	}
}

// TestADOUpdateWorkItemCloseHonoursExpectedRevision pins that a close
// pinned to the caller's revision does not retry over a newer edit: the
// conflict surfaces as RevisionConflictError after one PATCH.
func TestADOUpdateWorkItemCloseHonoursExpectedRevision(t *testing.T) {
	fake := &adoCloseFake{
		t: t, id: "23", itemType: "Issue", states: adoTestIssueStates,
		reads:       [][2]string{{"Active", "goobers/status:claimed"}},
		patchStatus: []int{http.StatusPreconditionFailed},
		patchBody:   `{"message":"rev mismatch"}`,
	}
	server := fake.server()
	defer server.Close()

	_, err := newADOCloseTestProvider(server).UpdateWorkItem(context.Background(), UpdateWorkItemRequest{
		Repository: adoCloseTestRepo, ID: "23", State: "closed", ExpectedRevision: "1",
	})
	var conflict *RevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want RevisionConflictError", err)
	}
	if conflict.Expected != "1" || conflict.Actual != "2" {
		t.Fatalf("conflict = %#v, want expected 1, actual 2", conflict)
	}
	if len(fake.patches) != 1 {
		t.Fatalf("patches = %d, want 1 (no retry over the caller's revision)", len(fake.patches))
	}
}

// TestADOCloseWorkItemStopsAtResolvedWithoutCompletedState pins the
// no-Completed-state case: an item at Resolved on a type without a
// Completed state is success, left at Resolved, and the note explaining the
// stop is posted once however often the close repeats.
func TestADOCloseWorkItemStopsAtResolvedWithoutCompletedState(t *testing.T) {
	fake := &adoCloseFake{
		t: t, id: "9", itemType: "Feedback Request",
		states: []map[string]string{
			{"name": "New", "category": "Proposed"},
			{"name": "Active", "category": "InProgress"},
			{"name": "Resolved", "category": "Resolved"},
		},
		reads: [][2]string{{"Resolved", "goobers/status:in-progress"}},
	}
	server := fake.server()
	defer server.Close()
	provider := newADOCloseTestProvider(server)

	for range 2 {
		item, err := provider.UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
			Repository: adoCloseTestRepo, ID: "9", Status: WorkItemStatusDone,
		})
		if err != nil {
			t.Fatalf("UpdateWorkItemStatus: %v", err)
		}
		if item.State != "open" {
			t.Fatalf("state = %q, want open (still Resolved)", item.State)
		}
	}
	for _, patch := range fake.patches {
		if _, ok := adoPatchValue(patch, "/fields/System.State"); ok {
			t.Fatalf("patch = %#v, want no System.State op", patch)
		}
	}
	if len(fake.comments) != 1 || !strings.Contains(fake.comments[0], adoResolvedStopMarker) {
		t.Fatalf("comments = %#v, want exactly one noting the Resolved stop", fake.comments)
	}
}

// TestADOCloseWorkItemStopsAtResolvedWhenTransitionRefused pins the refused
// transition: a Bug at Resolved whose type has a Completed state, but whose
// Resolved→Completed PATCH the server rejects with a rule error, stops at
// Resolved as success; the status tag still lands on a resend without the
// state op, and the note is posted.
func TestADOCloseWorkItemStopsAtResolvedWhenTransitionRefused(t *testing.T) {
	fake := &adoCloseFake{
		t: t, id: "7", itemType: "Bug",
		states: []map[string]string{
			{"name": "New", "category": "Proposed"},
			{"name": "Active", "category": "InProgress"},
			{"name": "Resolved", "category": "Resolved"},
			{"name": "Closed", "category": "Completed"},
		},
		reads:       [][2]string{{"Resolved", "goobers/status:in-progress"}},
		patchStatus: []int{http.StatusBadRequest},
		patchBody:   `{"message":"The field 'State' contains the value 'Closed' that is not in the list of supported values"}`,
	}
	server := fake.server()
	defer server.Close()

	item, err := newADOCloseTestProvider(server).UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: adoCloseTestRepo, ID: "7", Status: WorkItemStatusDone,
	})
	if err != nil {
		t.Fatalf("UpdateWorkItemStatus: %v", err)
	}
	if item.State != "open" {
		t.Fatalf("state = %q, want open (still Resolved)", item.State)
	}
	if len(fake.patches) != 2 {
		t.Fatalf("patches = %d, want 2 (refused close, then tag-only resend)", len(fake.patches))
	}
	if state, ok := adoPatchValue(fake.patches[0], "/fields/System.State"); !ok || state != "Closed" {
		t.Fatalf("first patch = %#v, want System.State=Closed", fake.patches[0])
	}
	resend := fake.patches[1]
	if _, ok := adoPatchValue(resend, "/fields/System.State"); ok {
		t.Fatalf("resend = %#v, want no System.State op", resend)
	}
	if tags, _ := adoPatchValue(resend, "/fields/System.Tags"); tags != "goobers/status:done" {
		t.Fatalf("resend tags = %q, want the done status tag", tags)
	}
	if len(fake.comments) != 1 || !strings.Contains(fake.comments[0], adoResolvedStopMarker) {
		t.Fatalf("comments = %#v, want one noting the Resolved stop", fake.comments)
	}
}

// TestADOCloseWorkItemRefusedFromActiveStillFails pins the boundary of the
// Resolved stop: a rule error on a close from an active state is not a
// Resolved stop and still fails the close.
func TestADOCloseWorkItemRefusedFromActiveStillFails(t *testing.T) {
	fake := &adoCloseFake{
		t: t, id: "8", itemType: "Issue", states: adoTestIssueStates,
		reads:       [][2]string{{"Active", "goobers/status:in-progress"}},
		patchStatus: []int{http.StatusBadRequest},
		patchBody:   `{"message":"rule error"}`,
	}
	server := fake.server()
	defer server.Close()

	_, err := newADOCloseTestProvider(server).UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: adoCloseTestRepo, ID: "8", Status: WorkItemStatusDone,
	})
	if err == nil {
		t.Fatalf("UpdateWorkItemStatus: want the rule error, got success")
	}
	if len(fake.patches) != 1 {
		t.Fatalf("patches = %d, want 1", len(fake.patches))
	}
}

// TestADOCloseWorkItemResolvedNoteFailureIsNotAnError pins that the
// Resolved-stop note is informational: a failure to post it leaves the
// close a success.
func TestADOCloseWorkItemResolvedNoteFailureIsNotAnError(t *testing.T) {
	fake := &adoCloseFake{
		t: t, id: "10", itemType: "Feedback Request",
		states: []map[string]string{
			{"name": "Active", "category": "InProgress"},
			{"name": "Resolved", "category": "Resolved"},
		},
		reads:       [][2]string{{"Resolved", "goobers/status:in-progress"}},
		commentFail: true,
	}
	server := fake.server()
	defer server.Close()

	if _, err := newADOCloseTestProvider(server).UpdateWorkItemStatus(context.Background(), UpdateWorkItemStatusRequest{
		Repository: adoCloseTestRepo, ID: "10", Status: WorkItemStatusDone,
	}); err != nil {
		t.Fatalf("UpdateWorkItemStatus: %v, want success despite the failed note", err)
	}
}
