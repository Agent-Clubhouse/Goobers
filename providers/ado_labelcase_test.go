package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/labelpredicate"
)

// ADO-N11: ADO tags and PR labels share one case-insensitive namespace whose
// casing is set by whoever wrote the tag first. These tests pin that the ADO
// provider compares them case-insensitively and folds what it reads onto the
// spelling Goobers compares exactly.

func TestCanonicalADOLabel(t *testing.T) {
	cases := []struct {
		label  string
		wanted []string
		want   string
	}{
		{"GOOBERS:READY", nil, "goobers:ready"},
		{"Goobers/Status:In-Progress", nil, "goobers/status:in-progress"},
		{"Goobers:Merge-Escalated", nil, "goobers:merge-escalated"},
		{"Tracking", nil, LabelTracking},
		{"STALE", nil, LabelStale},
		// A Goobers-namespace label Goobers does not own keeps ADO's casing,
		// so a config naming it in that spelling still matches exactly.
		{"goobers:Hold", nil, "goobers:Hold"},
		{"GOOBERS:Custom", nil, "GOOBERS:Custom"},
		{"Needs-Design", nil, "Needs-Design"},
		{"needs-design", []string{"Needs-Design"}, "Needs-Design"},
		{"GOOBERS:Custom", []string{"goobers:Custom"}, "goobers:Custom"},
	}
	for _, tc := range cases {
		if got := canonicalADOLabel(tc.label, tc.wanted); got != tc.want {
			t.Errorf("canonicalADOLabel(%q, %v) = %q, want %q", tc.label, tc.wanted, got, tc.want)
		}
	}
	got := canonicalADOLabels([]string{"GOOBERS:READY", "goobers:ready", "Keep"}, nil)
	if want := []string{"goobers:ready", "Keep"}; !slices.Equal(got, want) {
		t.Fatalf("canonicalADOLabels = %v, want %v (folded duplicates collapse)", got, want)
	}
}

func TestApplyADOTagSetIgnoresCase(t *testing.T) {
	got := applyADOTagSet(
		[]string{"GOOBERS:CLAIMED", "Route/Backend", "Goobers/Status:Claimed"},
		[]string{"goobers:claimed", "goobers/status:in-progress"},
		[]string{"goobers/status:claimed"},
	)
	want := []string{"GOOBERS:CLAIMED", "Route/Backend", "goobers/status:in-progress"}
	if !slices.Equal(got, want) {
		t.Fatalf("applyADOTagSet = %v, want %v", got, want)
	}
	if got := adoDropStatusTags([]string{"Goobers/Status:Open", "keep"}); !slices.Equal(got, []string{"keep"}) {
		t.Fatalf("adoDropStatusTags = %v, want [keep]", got)
	}
}

// TestGoobersOwnedLabelsCoverCommandLabels is the drift guard for
// goobersOwnedLabels: every label constant cmd/goobers declares must be
// listed, or the ADO provider reads it back in whatever casing ADO holds it.
func TestGoobersOwnedLabelsCoverCommandLabels(t *testing.T) {
	sources, err := filepath.Glob(filepath.Join("..", "cmd", "goobers", "*.go"))
	if err != nil || len(sources) == 0 {
		t.Fatalf("glob cmd/goobers sources: %v (found %d)", err, len(sources))
	}
	decl := regexp.MustCompile(`(?m)^\s*(?:const\s+)?\w+\s*=\s*"(goobers:[a-z0-9-]+|goobers/status:[a-z0-9-]+)"\s*$`)
	found := 0
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, match := range decl.FindAllStringSubmatch(string(src), -1) {
			found++
			if !slices.Contains(goobersOwnedLabels, match[1]) {
				t.Errorf("%s declares label %q, missing from goobersOwnedLabels", path, match[1])
			}
		}
	}
	if found == 0 {
		t.Fatal("found no cmd/goobers label constants; the guard's pattern no longer matches their declarations")
	}
}

// TestADOGetWorkItemFoldsOnlyGoobersOwnedLabels: GetWorkItem has no request
// labels to fold onto, so it folds onto the labels Goobers owns (including
// the unprefixed stale and tracking) and leaves every other tag as ADO holds
// it.
func TestADOGetWorkItemFoldsOnlyGoobersOwnedLabels(t *testing.T) {
	fake := &adoClaimFake{tags: "Tracking; Stale; GOOBERS:READY; goobers:Hold; Needs-Design"}
	server := fake.server(t, true)
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	item, err := provider.GetWorkItem(context.Background(), RepositoryRef{Name: "repo", Project: "project"}, "42")
	if err != nil {
		t.Fatalf("GetWorkItem: %v", err)
	}
	for _, label := range []string{LabelTracking, LabelStale, LabelReady, "goobers:Hold", "Needs-Design"} {
		if !item.HasLabel(label) {
			t.Errorf("HasLabel(%q) = false, labels = %v", label, item.Labels)
		}
	}
}

// TestADOListWorkItemsFoldsCompareLabels: a caller that applies its label
// predicate itself names the predicate's labels in CompareLabels, and a tag
// first written in another casing reads back in that spelling.
func TestADOListWorkItemsFoldsCompareLabels(t *testing.T) {
	_, server := newADOBatchTestServer(t, 1, func(int) string { return "GOOBERS:READY; Needs-Design" })
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository:    RepositoryRef{Name: "repo", Project: "project"},
		Labels:        []string{LabelReady},
		CompareLabels: []string{"needs-design"},
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 1 || !items[0].HasLabel(LabelReady) || !items[0].HasLabel("needs-design") {
		t.Fatalf("items = %#v, want one item carrying goobers:ready and needs-design", items)
	}
}

// TestADOListWorkItemsMatchesTagWrittenInOtherCase: a human created the tag
// as GOOBERS:READY first, so ADO returns that casing. requireLabels and the
// label predicate still match, and the item carries goobers:ready.
func TestADOListWorkItemsMatchesTagWrittenInOtherCase(t *testing.T) {
	_, server := newADOBatchTestServer(t, 2, func(id int) string {
		if id == 1 {
			return "GOOBERS:READY; Team-A"
		}
		return "other"
	})
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	predicate, err := labelpredicate.Compile(`"goobers:ready" in labels && "team-a" in labels`, []string{LabelReady}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	items, err := provider.ListWorkItems(context.Background(), ListWorkItemsRequest{
		Repository:     RepositoryRef{Name: "repo", Project: "project"},
		Labels:         []string{LabelReady},
		LabelPredicate: predicate,
	})
	if err != nil {
		t.Fatalf("ListWorkItems: %v", err)
	}
	if len(items) != 1 || items[0].ID != "1" {
		t.Fatalf("items = %v, want only item 1", workItemIDs(items))
	}
	if !items[0].HasLabel(LabelReady) || !items[0].HasLabel("team-a") {
		t.Fatalf("labels = %v, want goobers:ready and team-a (requested spelling)", items[0].Labels)
	}
}

// TestADOClaimIsIdempotentOverMixedCaseClaimTag: goobers:claimed already
// exists as GOOBERS:CLAIMED. The claim neither writes a duplicate tag nor
// fails its visibility check.
func TestADOClaimIsIdempotentOverMixedCaseClaimTag(t *testing.T) {
	fake := &adoClaimFake{tags: "GOOBERS:CLAIMED"}
	result, err := claimADOTestItem(t, fake.server(t, true), "run-ours")
	if err != nil {
		t.Fatalf("ClaimWorkItem: %v", err)
	}
	if !result.Claimed || !result.Item.HasLabel(LabelClaimed) {
		t.Fatalf("claim = %#v, want claimed with goobers:claimed visible", result)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, written := range fake.patchedTags {
		if n := strings.Count(strings.ToLower(written), LabelClaimed); n != 1 {
			t.Fatalf("tag write %q carries the claim tag %d times, want once", written, n)
		}
	}
}

// TestADOClaimCustomLabelVisibleInFirstWriterCase: a custom claim label
// outside the Goobers namespace keeps ADO's first-writer casing, and the
// claim's visibility check still finds it.
func TestADOClaimCustomLabelVisibleInFirstWriterCase(t *testing.T) {
	fake := &adoClaimFake{tags: "Example-Claimed"}
	server := fake.server(t, true)
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	result, err := provider.ClaimWorkItem(context.Background(), ClaimWorkItemRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		ID:         "42",
		RunID:      "run-ours",
		ClaimLabel: "example-claimed",
	})
	if err != nil {
		t.Fatalf("ClaimWorkItem: %v", err)
	}
	if !result.Claimed {
		t.Fatalf("claim = %#v, want claimed", result)
	}
}

// TestADOReleaseRemovesMixedCaseClaimTag: release clears the claim tag
// whatever casing ADO holds it in.
func TestADOReleaseRemovesMixedCaseClaimTag(t *testing.T) {
	fake := &adoClaimFake{tags: "GOOBERS:CLAIMED; keep"}
	fake.seed(adoTestSelfID, claimBreadcrumb("run-ours"))
	server := fake.server(t, true)
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	item, err := provider.ReleaseWorkItemClaim(context.Background(), ClaimWorkItemRequest{
		Repository: RepositoryRef{Name: "repo", Project: "project"},
		ID:         "42",
		RunID:      "run-ours",
	})
	if err != nil {
		t.Fatalf("ReleaseWorkItemClaim: %v", err)
	}
	if item.HasLabel(LabelClaimed) {
		t.Fatalf("labels after release = %v, want goobers:claimed gone", item.Labels)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.tags != "keep" {
		t.Fatalf("stored tags = %q, want %q", fake.tags, "keep")
	}
}

// adoPRLabelsFake serves one PR's labels endpoint: GET lists labels, POST
// adds one (failing any name in failPost), DELETE by id removes one.
type adoPRLabelsFake struct {
	mu       sync.Mutex
	labels   []adoPullRequestLabel
	failPost map[string]bool
	failGet  bool
	posted   []string
	deleted  []string
}

func (f *adoPRLabelsFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	const base = "/org/project/_apis/git/repositories/repo/pullrequests/42/labels"
	mux := http.NewServeMux()
	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if f.failGet {
				http.Error(w, "label read refused", http.StatusBadRequest)
				return
			}
			values := make([]map[string]string, 0, len(f.labels))
			for _, l := range f.labels {
				values = append(values, map[string]string{"id": l.ID, "name": l.Name})
			}
			writeJSON(t, w, map[string]interface{}{"value": values})
		case http.MethodPost:
			var body struct {
				Name string `json:"name"`
			}
			decodeJSON(t, r, &body)
			f.posted = append(f.posted, body.Name)
			if f.failPost[body.Name] {
				http.Error(w, "label write refused", http.StatusBadRequest)
				return
			}
			f.labels = append(f.labels, adoPullRequestLabel{ID: "id-" + body.Name, Name: body.Name})
			writeJSON(t, w, map[string]string{"id": "id-" + body.Name, "name": body.Name})
		default:
			http.Error(w, "unsupported", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc(base+"/", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodDelete)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deleted = append(f.deleted, strings.TrimPrefix(r.URL.Path, base+"/"))
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func newADOPRLabelsTestProvider(server *httptest.Server) *ADOProvider {
	return NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
}

var adoPRLabelsTestRepo = RepositoryRef{Name: "repo", Project: "project"}

func TestADOPullRequestLabelNamesKeepsCase(t *testing.T) {
	fake := &adoPRLabelsFake{labels: []adoPullRequestLabel{
		{ID: "1", Name: "Needs-Design"},
		{ID: "2", Name: "GOOBERS:Merge-Escalated"},
	}}
	names, err := newADOPRLabelsTestProvider(fake.server(t)).PullRequestLabelNames(context.Background(), adoPRLabelsTestRepo, "42")
	if err != nil {
		t.Fatalf("PullRequestLabelNames: %v", err)
	}
	if want := []string{"Needs-Design", "goobers:merge-escalated"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v (human casing kept, Goobers namespace folded)", names, want)
	}
}

// TestADOAddPullRequestLabelsReportsPartialSuccess is the PO acceptance test
// for the partial add: the second of three labels fails, the first and third
// are applied and kept, and the error names only the second.
func TestADOAddPullRequestLabelsReportsPartialSuccess(t *testing.T) {
	fake := &adoPRLabelsFake{failPost: map[string]bool{"label-b": true}}
	err := newADOPRLabelsTestProvider(fake.server(t)).AddPullRequestLabels(
		context.Background(), adoPRLabelsTestRepo, "42", []string{"label-a", "label-b", "label-c"})
	var partial *PullRequestLabelAddError
	if !errors.As(err, &partial) {
		t.Fatalf("AddPullRequestLabels error = %v, want *PullRequestLabelAddError", err)
	}
	if !slices.Equal(partial.Applied, []string{"label-a", "label-c"}) || !slices.Equal(partial.Failed, []string{"label-b"}) {
		t.Fatalf("applied = %v, failed = %v, want [label-a label-c] / [label-b]", partial.Applied, partial.Failed)
	}
	msg := err.Error()
	if !strings.Contains(msg, `"label-b"`) || strings.Contains(msg, "label-a") || strings.Contains(msg, "label-c") {
		t.Fatalf("error = %q, want it to name only label-b", msg)
	}
	var responseErr *providerResponseError
	if !errors.As(err, &responseErr) {
		t.Fatalf("error = %v, want the failed POST's cause reachable through errors.As", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted = %v, want nothing rolled back", fake.deleted)
	}
	if !slices.Equal(fake.posted, []string{"label-a", "label-b", "label-c"}) {
		t.Fatalf("posted = %v, want all three attempted in order", fake.posted)
	}
}

// TestADOAddPullRequestLabelsProceedsWhenPreReadFails: a failed read of the
// existing labels does not stop the adds.
func TestADOAddPullRequestLabelsProceedsWhenPreReadFails(t *testing.T) {
	fake := &adoPRLabelsFake{failGet: true, failPost: map[string]bool{"label-b": true}}
	provider := newADOPRLabelsTestProvider(fake.server(t))
	if err := provider.AddPullRequestLabels(context.Background(), adoPRLabelsTestRepo, "42", []string{"label-a"}); err != nil {
		t.Fatalf("AddPullRequestLabels: %v", err)
	}
	err := provider.AddPullRequestLabels(context.Background(), adoPRLabelsTestRepo, "42", []string{"label-b"})
	var partial *PullRequestLabelAddError
	if !errors.As(err, &partial) || !strings.Contains(err.Error(), "read existing pull request labels") {
		t.Fatalf("error = %v, want a *PullRequestLabelAddError joined with the read failure", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !slices.Equal(fake.posted, []string{"label-a", "label-b"}) {
		t.Fatalf("posted = %v, want both adds attempted", fake.posted)
	}
}

// TestADOVisibleLabelsHidesLegacyPrefixInAnyCase: adoVisibleLabels still
// filters a legacy owner tag regardless of the casing ADO holds its prefix
// in, keeping a stray pre-#1990 tag from surfacing as a routing label.
func TestADOVisibleLabelsHidesLegacyPrefixInAnyCase(t *testing.T) {
	mixed := strings.ToUpper(adoClaimTagPrefix) + "cnVuLU91cnM"
	visible := adoVisibleLabels([]string{"keep", mixed})
	if !slices.Equal(visible, []string{"keep"}) {
		t.Fatalf("adoVisibleLabels = %v, want legacy tag filtered", visible)
	}
}

func TestADOAddPullRequestLabelsSkipsLabelPresentInOtherCase(t *testing.T) {
	fake := &adoPRLabelsFake{labels: []adoPullRequestLabel{{ID: "1", Name: "GOOBERS:NEEDS-REMEDIATION"}}}
	err := newADOPRLabelsTestProvider(fake.server(t)).AddPullRequestLabels(
		context.Background(), adoPRLabelsTestRepo, "42",
		[]string{"goobers:needs-remediation", "goobers:merge-escalated", "Goobers:Merge-Escalated"})
	if err != nil {
		t.Fatalf("AddPullRequestLabels: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !slices.Equal(fake.posted, []string{"goobers:merge-escalated"}) {
		t.Fatalf("posted = %v, want only goobers:merge-escalated", fake.posted)
	}
}

func TestADORemovePullRequestLabelIgnoresCase(t *testing.T) {
	fake := &adoPRLabelsFake{labels: []adoPullRequestLabel{{ID: "label-guid", Name: "Goobers:Needs-Remediation"}}}
	err := newADOPRLabelsTestProvider(fake.server(t)).RemovePullRequestLabel(
		context.Background(), adoPRLabelsTestRepo, "42", "goobers:needs-remediation")
	if err != nil {
		t.Fatalf("RemovePullRequestLabel: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !slices.Equal(fake.deleted, []string{"label-guid"}) {
		t.Fatalf("deleted = %v, want [label-guid]", fake.deleted)
	}
}
