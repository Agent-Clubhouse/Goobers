package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
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
	update := adoTestUpdate(id, map[string]interface{}{
		"System.Tags":        tags,
		"System.ChangedDate": map[string]interface{}{"oldValue": "2026-01-01T00:00:00Z", "newValue": changed},
		// A real updates entry also carries System.RevisedDate as an
		// old/new field change. Its value here differs from both the change
		// date and the top-level revisedDate, so taking either revised date
		// as the transition time fails the test.
		"System.RevisedDate": map[string]interface{}{"oldValue": "9999-01-01T00:00:00Z", "newValue": "2025-06-01T00:00:00Z"},
	})
	// revisedDate also sits at the top level of a real updates entry, beside
	// id and rev: it is when the revision was superseded (9999-01-01 for the
	// latest), so the walk must not take it as the transition time either.
	update["revisedDate"] = "9999-01-01T00:00:00Z"
	return update
}

// adoTestUpdatesServer shapes serveADOTestUpdatesWith's fake: the item's
// current revision (0 means the last update's), a server-side $top cap (0
// means none), and how many updates it serves before answering empty pages
// (0 means all of them).
type adoTestUpdatesServer struct {
	currentRev int
	maxTop     int
	serveOnly  int
}

// serveADOTestUpdates serves updates for work item 42 honouring $top/$skip,
// and records each $skip requested. The item itself answers with the last
// update's revision, as ADO does.
func serveADOTestUpdates(t *testing.T, updates []map[string]interface{}, skips *[]int) *httptest.Server {
	t.Helper()
	return serveADOTestUpdatesWith(t, updates, skips, adoTestUpdatesServer{})
}

func serveADOTestUpdatesWith(t *testing.T, updates []map[string]interface{}, skips *[]int, shape adoTestUpdatesServer) *httptest.Server {
	t.Helper()
	currentRev := shape.currentRev
	if currentRev == 0 && len(updates) > 0 {
		currentRev = updates[len(updates)-1]["rev"].(int)
	}
	served := updates
	if shape.serveOnly > 0 {
		served = updates[:shape.serveOnly]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/workitems/42", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"id": 42, "rev": currentRev, "fields": map[string]interface{}{"System.Id": 42}})
	})
	mux.HandleFunc("/org/project/_apis/wit/workItems/42/updates", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		top, err := strconv.Atoi(r.URL.Query().Get("$top"))
		if err != nil || top <= 0 {
			t.Fatalf("$top = %q, want a positive page size", r.URL.Query().Get("$top"))
		}
		if shape.maxTop > 0 && top > shape.maxTop {
			top = shape.maxTop
		}
		skip, err := strconv.Atoi(r.URL.Query().Get("$skip"))
		if err != nil {
			t.Fatalf("$skip = %q, want an integer", r.URL.Query().Get("$skip"))
		}
		*skips = append(*skips, skip)
		end := skip + top
		if end > len(served) {
			end = len(served)
		}
		page := []map[string]interface{}{}
		if skip < len(served) {
			page = served[skip:end]
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
	if !errors.Is(err, ErrLabelHistoryIncomplete) {
		t.Fatalf("revision-cap error = %v, want it to wrap ErrLabelHistoryIncomplete so a claim stage can skip the item", err)
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
	_, err := provider.ListWorkItemLabelTransitionsForItem(context.Background(),
		RepositoryRef{Name: "repo", Project: "project"}, "42", LabelReady)
	if err == nil || !strings.Contains(err.Error(), "System.ChangedDate") {
		t.Fatalf("ListWorkItemLabelTransitionsForItem error = %v, want a missing System.ChangedDate error", err)
	}
	if !errors.Is(err, ErrLabelHistoryIncomplete) {
		t.Fatalf("missing-ChangedDate error = %v, want it to wrap ErrLabelHistoryIncomplete", err)
	}
}

// adoTestReadyHistory is a history whose ready tag is added at rev 2, removed
// late at rev 160 and re-added at rev 170 — so a walk that stops early
// reports the original add as the item's current readiness.
func adoTestReadyHistory(total int) []map[string]interface{} {
	updates := make([]map[string]interface{}, 0, total)
	for i := 1; i <= total; i++ {
		switch i {
		case 2:
			updates = append(updates, adoTestTagUpdate(i, "", LabelReady, "2026-02-02T00:00:00Z"))
		case 160:
			updates = append(updates, adoTestTagUpdate(i, LabelReady, "", "2026-03-01T00:00:00Z"))
		case 170:
			updates = append(updates, adoTestTagUpdate(i, "", LabelReady, "2026-03-02T00:00:00Z"))
		default:
			updates = append(updates, adoTestUpdate(i, map[string]interface{}{
				"System.ChangedDate": map[string]interface{}{"newValue": "2026-02-03T00:00:00Z"},
			}))
		}
	}
	return updates
}

// TestADOLabelTransitionsWalkSurvivesServerCappedPageSize pins #5960: a
// server that serves fewer than the requested $top must not end the walk at
// its first (short) page — the walk pages on by what each page returned until
// it reaches the item's current revision, and so still sees the late re-add.
func TestADOLabelTransitionsWalkSurvivesServerCappedPageSize(t *testing.T) {
	var skips []int
	server := serveADOTestUpdatesWith(t, adoTestReadyHistory(180), &skips, adoTestUpdatesServer{maxTop: 50})
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	got, err := provider.ListWorkItemLabelTransitionsForItem(context.Background(),
		RepositoryRef{Name: "repo", Project: "project"}, "42", LabelReady)
	if err != nil {
		t.Fatalf("ListWorkItemLabelTransitionsForItem: %v", err)
	}
	if want := []int{0, 50, 100, 150}; !slices.Equal(skips, want) {
		t.Fatalf("requested $skip = %v, want %v (advance by the served page, stop at the current revision)", skips, want)
	}
	if len(got) != 3 || got[2].EventID != 170 || !got[2].Added {
		t.Fatalf("transitions = %+v, want add@2, remove@160, re-add@170", got)
	}
}

// TestADOLabelTransitionsWalkStopsAtCurrentRevisionWithoutEmptyPage: a
// single-page history costs the revision read and one page, not a trailing
// empty-page request.
func TestADOLabelTransitionsWalkStopsAtCurrentRevisionWithoutEmptyPage(t *testing.T) {
	var skips []int
	server := serveADOTestUpdates(t, adoTestReadyHistory(3), &skips)
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	if _, err := provider.ListWorkItemLabelTransitionsForItem(context.Background(),
		RepositoryRef{Name: "repo", Project: "project"}, "42", LabelReady); err != nil {
		t.Fatalf("ListWorkItemLabelTransitionsForItem: %v", err)
	}
	if !slices.Equal(skips, []int{0}) {
		t.Fatalf("requested $skip = %v, want only [0]", skips)
	}
}

// TestADOLabelTransitionsWalkFailsClosedShortOfCurrentRevision pins #5960's
// fail-closed half: a history whose pages run out (an empty page) before the
// item's current revision is truncated, and must not be read as complete.
func TestADOLabelTransitionsWalkFailsClosedShortOfCurrentRevision(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape adoTestUpdatesServer
	}{
		{name: "server stops serving updates", shape: adoTestUpdatesServer{serveOnly: 120}},
		{name: "item revision beyond its history", shape: adoTestUpdatesServer{currentRev: 181}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var skips []int
			server := serveADOTestUpdatesWith(t, adoTestReadyHistory(180), &skips, tc.shape)
			defer server.Close()

			provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
			got, err := provider.ListWorkItemLabelTransitionsForItem(context.Background(),
				RepositoryRef{Name: "repo", Project: "project"}, "42", LabelReady)
			if !errors.Is(err, ErrLabelHistoryIncomplete) || !errors.Is(err, ErrLabelHistoryRevisionMismatch) {
				t.Fatalf("ListWorkItemLabelTransitionsForItem = %+v, %v; want ErrLabelHistoryRevisionMismatch (an ErrLabelHistoryIncomplete)", got, err)
			}
			if !strings.Contains(err.Error(), "before its current revision") {
				t.Fatalf("error = %v, want the revision mismatch named", err)
			}
		})
	}
}

// TestADOLabelTransitionsWalkAcceptsHistoryNewerThanTheRevisionRead: an item
// edited between the revision read and the walk has a longer history than
// the anchor, which is complete, not truncated.
func TestADOLabelTransitionsWalkAcceptsHistoryNewerThanTheRevisionRead(t *testing.T) {
	var skips []int
	server := serveADOTestUpdatesWith(t, adoTestReadyHistory(180), &skips, adoTestUpdatesServer{currentRev: 175})
	defer server.Close()

	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	got, err := provider.ListWorkItemLabelTransitionsForItem(context.Background(),
		RepositoryRef{Name: "repo", Project: "project"}, "42", LabelReady)
	if err != nil || len(got) != 3 {
		t.Fatalf("ListWorkItemLabelTransitionsForItem = %+v, %v; want three transitions", got, err)
	}
}
