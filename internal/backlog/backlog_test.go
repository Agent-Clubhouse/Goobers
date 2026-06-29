package backlog

import (
	"reflect"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// TestFromWorkItem_CarriesWireFields verifies the six wire-contract fields are
// copied through exactly.
func TestFromWorkItem_CarriesWireFields(t *testing.T) {
	w := providers.WorkItem{
		Provider: providers.ProviderGitHub,
		ID:       "42",
		Title:    "Add retry to webhook delivery",
		Body:     "Deliveries should retry with backoff.",
		URL:      "https://github.com/acme/web/issues/42",
		Labels:   []string{"goobers", "bug"},
	}

	got := FromWorkItem(w)

	want := apiv1.BacklogItem{
		ID:       "42",
		Provider: apiv1.ProviderGitHub,
		Title:    "Add retry to webhook delivery",
		Body:     "Deliveries should retry with backoff.",
		URL:      "https://github.com/acme/web/issues/42",
		Labels:   []string{"goobers", "bug"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FromWorkItem mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// TestFromWorkItem_ADOProvider verifies the provider kind converts across the
// string boundary for the non-default backend too.
func TestFromWorkItem_ADOProvider(t *testing.T) {
	got := FromWorkItem(providers.WorkItem{Provider: providers.ProviderADO, ID: "AB#7"})
	if got.Provider != apiv1.ProviderADO {
		t.Errorf("provider = %q, want %q", got.Provider, apiv1.ProviderADO)
	}
	if string(got.Provider) != string(providers.ProviderADO) {
		t.Errorf("provider string value not preserved: %q vs %q", got.Provider, providers.ProviderADO)
	}
}

// TestFromWorkItem_DropsProviderOnlyFields is the guard for the intentional
// lossy projection: provider-layer fields must NOT leak into the wire envelope.
// If the wire contract ever grows a field, this test forces a deliberate update.
func TestFromWorkItem_DropsProviderOnlyFields(t *testing.T) {
	now := time.Now()
	w := providers.WorkItem{
		Provider:   providers.ProviderGitHub,
		ID:         "1",
		ExternalID: "ext-1",
		Type:       "bug",
		Title:      "t",
		Body:       "b",
		Labels:     []string{"x"},
		State:      "open",
		Status:     providers.WorkItemStatusInProgress,
		Assignee:   "alice",
		Links:      []providers.Link{{Rel: "self", URL: "u"}},
		Parent:     &providers.WorkItemRef{Provider: providers.ProviderGitHub, ID: "0"},
		Hierarchy:  map[string]interface{}{"depth": 1},
		URL:        "u",
		UpdatedAt:  &now,
		Raw:        map[string]interface{}{"native": true},
	}

	got := FromWorkItem(w)

	// The BacklogItem struct has exactly six fields; assert no provider-only
	// data is representable on it. We check this structurally so the test breaks
	// loudly if BacklogItem's shape changes.
	if n := reflect.TypeOf(got).NumField(); n != 6 {
		t.Fatalf("BacklogItem now has %d fields; reassess the projection and this guard", n)
	}
	// Spot-check the carried fields survived alongside the drops.
	if got.ID != "1" || got.Title != "t" || got.Body != "b" || got.URL != "u" {
		t.Errorf("carried fields corrupted: %+v", got)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "x" {
		t.Errorf("labels = %v, want [x]", got.Labels)
	}
}

// TestFromWorkItem_EmptyAndNilSlices verifies a zero work item yields a zero
// wire item (no spurious allocation), so renders/JSON stay stable.
func TestFromWorkItem_EmptyAndNilSlices(t *testing.T) {
	got := FromWorkItem(providers.WorkItem{})
	if got.ID != "" || got.Title != "" || got.Body != "" || got.URL != "" {
		t.Errorf("zero work item should yield zero string fields, got %+v", got)
	}
	if got.Labels != nil {
		t.Errorf("nil labels should pass through as nil, got %v", got.Labels)
	}
	if got.Provider != "" {
		t.Errorf("zero provider should be empty, got %q", got.Provider)
	}
}

// TestFromWorkItem_LabelsAliasNotCopied documents that Labels is passed by
// reference (slice header), matching the prior ad-hoc bridge's behavior — no
// behavior change. Callers that mutate must copy; the scheduler does not mutate.
func TestFromWorkItem_LabelsAliasNotCopied(t *testing.T) {
	labels := []string{"a", "b"}
	got := FromWorkItem(providers.WorkItem{Labels: labels})
	if &got.Labels[0] != &labels[0] {
		t.Error("expected Labels to alias the source slice (parity with prior bridge)")
	}
}
