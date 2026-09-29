package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// adoTestUpdate builds one work-item update carrying the given field changes.
func adoTestUpdate(id int, fields map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"id": id, "rev": id, "fields": fields}
}

func adoTestTagUpdate(id int, oldTags, newTags, changed string) map[string]interface{} {
	tags := map[string]interface{}{}
	if oldTags != "" {
		tags["oldValue"] = oldTags
	}
	if newTags != "" {
		tags["newValue"] = newTags
	}
	return adoTestUpdate(id, map[string]interface{}{
		"System.Tags":        tags,
		"System.ChangedDate": map[string]interface{}{"oldValue": "2026-01-01T00:00:00Z", "newValue": changed},
		// revisedDate is when the revision was superseded; the walk must not
		// take it as the transition time.
		"System.RevisedDate": map[string]interface{}{"newValue": "9999-01-01T00:00:00Z"},
	})
}

// serveADOTestUpdates serves updates for work item 42 honouring $top/$skip,
// and records each $skip requested.
func serveADOTestUpdates(t *testing.T, updates []map[string]interface{}, skips *[]int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/workItems/42/updates", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		top, err := strconv.Atoi(r.URL.Query().Get("$top"))
		if err != nil || top <= 0 {
			t.Fatalf("$top = %q, want a positive page size", r.URL.Query().Get("$top"))
		}
		skip, err := strconv.Atoi(r.URL.Query().Get("$skip"))
		if err != nil {
			t.Fatalf("$skip = %q, want an integer", r.URL.Query().Get("$skip"))
		}
		*skips = append(*skips, skip)
		end := skip + top
		if end > len(updates) {
			end = len(updates)
		}
		page := []map[string]interface{}{}
		if skip < len(updates) {
			page = updates[skip:end]
		}
		writeJSON(t, w, map[string]interface{}{"count": len(page), "value": page})
	})
	return httptest.NewServer(mux)
}

// TestADOListWorkItemLabelTransitionsForItemDiffsTagUpdates pins ADO-N21: the
// label's history is read from the work item's updates across pages, each
// System.Tags old/new diff yields an add or a remove matched ignoring case,
// untouched updates are skipped, and the time is System.ChangedDate.
func TestADOListWorkItemLabelTransitionsForItemDiffsTagUpdates(t *testing.T) {
	updates := make([]map[string]interface{}, 0, adoUpdatesPageSize+10)
	updates = append(updates, adoTestUpdate(1, map[string]interface{}{
		"System.Title":       map[string]interface{}{"newValue": "created"},
		"System.ChangedDate": map[string]interface{}{"newValue": "2026-02-01T00:00:00Z"},
	}))
	updates = append(updates, adoTestTagUpdate(2, "", "area/backend; GOOBERS:READY", "2026-02-02T10:00:00.123Z"))
	for i := len(updates) + 1; i <= adoUpdatesPageSize+2; i++ {
		updates = append(updates, adoTestUpdate(i, map[string]interface{}{
			"System.Title":       map[string]interface{}{"newValue": "edit " + strconv.Itoa(i)},
			"System.ChangedDate": map[string]interface{}{"newValue": "2026-02-03T00:00:00Z"},
		}))
	}
	// Second page: an unrelated tag edit, a removal, then a re-add in another casing.
	updates = append(updates,
		adoTestTagUpdate(adoUpdatesPageSize+3, "area/backend; GOOBERS:READY", "GOOBERS:READY; area/frontend", "2026-02-04T00:00:00Z"),
		adoTestTagUpdate(adoUpdatesPageSize+4, "GOOBERS:READY; area/frontend", "area/frontend", "2026-02-05T00:00:00Z"),
		adoTestTagUpdate(adoUpdatesPageSize+5, "area/frontend", "area/frontend; goobers:Ready", "2026-02-06T00:00:00Z"),
	)
	var skips []int
	server := serveADOTestUpdates(t, updates, &skips)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	got, err := provider.ListWorkItemLabelTransitionsForItem(context.Background(),
		RepositoryRef{Name: "repo", Project: "project"}, "42", LabelReady)
	if err != nil {
		t.Fatalf("ListWorkItemLabelTransitionsForItem: %v", err)
	}
	if len(skips) != 2 || skips[0] != 0 || skips[1] != adoUpdatesPageSize {
		t.Fatalf("requested $skip = %v, want [0 %d]", skips, adoUpdatesPageSize)
	}
	want := []WorkItemLabelTransition{
		{EventID: 2, Added: true, OccurredAt: time.Date(2026, 2, 2, 10, 0, 0, 123000000, time.UTC)},
		{EventID: int64(adoUpdatesPageSize + 4), Added: false, OccurredAt: time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)},
		{EventID: int64(adoUpdatesPageSize + 5), Added: true, OccurredAt: time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)},
	}
	if len(got) != len(want) {
		t.Fatalf("transitions = %+v, want %d entries", got, len(want))
	}
	for i := range want {
		if got[i].EventID != want[i].EventID || got[i].Added != want[i].Added ||
			!got[i].OccurredAt.Equal(want[i].OccurredAt) || got[i].ItemID != "42" || got[i].Label != LabelReady {
			t.Fatalf("transition[%d] = %+v, want %+v (item 42, label %q)", i, got[i], want[i], LabelReady)
		}
	}
}

// TestADOListWorkItemLabelTransitionsForItemFailsClosedAtRevisionCap pins the
// fail-closed walk: a history that reaches ADO's 10,000-revision cap cannot be
// read completely, so it errors instead of returning a truncated history.
func TestADOListWorkItemLabelTransitionsForItemFailsClosedAtRevisionCap(t *testing.T) {
	updates := make([]map[string]interface{}, 0, adoMaxWorkItemRevisions)
	updates = append(updates, adoTestTagUpdate(1, "", LabelReady, "2026-02-02T00:00:00Z"))
	for i := 2; i <= adoMaxWorkItemRevisions; i++ {
		updates = append(updates, adoTestUpdate(i, map[string]interface{}{
			"System.ChangedDate": map[string]interface{}{"newValue": "2026-02-03T00:00:00Z"},
		}))
	}
	var skips []int
	server := serveADOTestUpdates(t, updates, &skips)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	got, err := provider.ListWorkItemLabelTransitionsForItem(context.Background(),
		RepositoryRef{Name: "repo", Project: "project"}, "42", LabelReady)
	if err == nil || !strings.Contains(err.Error(), "revision cap") {
		t.Fatalf("ListWorkItemLabelTransitionsForItem = %+v, %v; want a revision-cap error", got, err)
	}
}

// TestADOListWorkItemLabelTransitionsForItemRequiresChangedDate pins that a
// tag change without System.ChangedDate fails rather than yielding a zero
// time a readiness age would misread.
func TestADOListWorkItemLabelTransitionsForItemRequiresChangedDate(t *testing.T) {
	updates := []map[string]interface{}{adoTestUpdate(1, map[string]interface{}{
		"System.Tags": map[string]interface{}{"newValue": LabelReady},
	})}
	var skips []int
	server := serveADOTestUpdates(t, updates, &skips)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	if _, err := provider.ListWorkItemLabelTransitionsForItem(context.Background(),
		RepositoryRef{Name: "repo", Project: "project"}, "42", LabelReady); err == nil ||
		!strings.Contains(err.Error(), "System.ChangedDate") {
		t.Fatalf("ListWorkItemLabelTransitionsForItem error = %v, want a missing System.ChangedDate error", err)
	}
}
