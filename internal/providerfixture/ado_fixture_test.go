package providerfixture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeADOBoard serves the work-item requests EnsureADOFixture and RefreshADO
// make against one project, acme/Widgets.
type fakeADOBoard struct {
	t     *testing.T
	mu    sync.Mutex
	items map[int]map[string]any
	next  int
	posts []string
}

func newFakeADOBoard(t *testing.T, next int) *fakeADOBoard {
	return &fakeADOBoard{t: t, items: map[int]map[string]any{}, next: next}
}

func (f *fakeADOBoard) add(id int, title, state string) {
	f.items[id] = map[string]any{
		"System.WorkItemType": "Issue",
		"System.Title":        title,
		"System.Description":  "Stable fixture body.",
		"System.State":        state,
		"System.Tags":         ADOFixtureTag,
		"System.CreatedDate":  "2026-07-01T01:02:03Z",
		"System.ChangedDate":  "2026-07-01T04:05:06Z",
	}
}

func (f *fakeADOBoard) item(id int) map[string]any {
	return map[string]any{
		"id":     id,
		"rev":    3,
		"url":    "https://dev.azure.com/acme/Widgets/_workitems/edit/" + strconv.Itoa(id),
		"fields": f.items[id],
		"_links": map[string]any{"self": map[string]any{
			"href": "https://dev.azure.com/acme/Widgets/_apis/wit/workItems/" + strconv.Itoa(id),
		}},
	}
}

func (f *fakeADOBoard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	path := strings.TrimPrefix(r.URL.Path, "/acme/Widgets/_apis/wit/")
	switch {
	case path == "wiql":
		ids := make([]int, 0, len(f.items))
		for id := range f.items {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		refs := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			refs = append(refs, map[string]any{"id": id, "url": "https://dev.azure.com/acme/Widgets/_apis/wit/workItems/" + strconv.Itoa(id)})
		}
		writeADOJSON(f.t, w, map[string]any{"asOf": "2026-07-01T00:00:00Z", "workItems": refs})
	case path == "workitemsbatch":
		var request struct {
			IDs []int `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			f.t.Errorf("decode workitemsbatch: %v", err)
		}
		value := make([]any, 0, len(request.IDs))
		for _, id := range request.IDs {
			value = append(value, f.item(id))
		}
		writeADOJSON(f.t, w, map[string]any{"count": len(value), "value": value})
	case path == "workitemtypes/Issue/states":
		writeADOJSON(f.t, w, map[string]any{"value": []map[string]string{
			{"name": "Active", "category": "InProgress"},
			{"name": "Closed", "category": "Completed"},
		}})
	case path == "workitems/$Issue" && r.Method == http.MethodPost:
		f.posts = append(f.posts, r.URL.Path)
		f.create(w, r)
	case strings.HasPrefix(path, "workitems/"):
		id, err := strconv.Atoi(strings.TrimPrefix(path, "workitems/"))
		if err != nil || f.items[id] == nil {
			http.NotFound(w, r)
			return
		}
		writeADOJSON(f.t, w, f.item(id))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeADOBoard) create(w http.ResponseWriter, r *http.Request) {
	var patch []struct {
		Path  string `json:"path"`
		Value any    `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		f.t.Errorf("decode create patch: %v", err)
	}
	fields := map[string]string{}
	for _, op := range patch {
		if value, ok := op.Value.(string); ok {
			fields[strings.TrimPrefix(op.Path, "/fields/")] = value
		}
	}
	if fields["System.Title"] != ADOFixtureTitle || fields["System.Tags"] != ADOFixtureTag ||
		!strings.Contains(fields["System.Description"], ADOFixtureBody) {
		f.t.Errorf("created fixture fields = %v", fields)
	}
	id := f.next
	f.next++
	f.add(id, fields["System.Title"], "Active")
	writeADOJSON(f.t, w, f.item(id))
}

func (f *fakeADOBoard) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func ensureFixtureConfig(srv *httptest.Server, provision bool) ADOFixtureConfig {
	return ADOFixtureConfig{
		OrganizationURL: srv.URL + "/acme",
		Project:         "Widgets",
		Token:           "ado-pat",
		Provision:       provision,
	}
}

func TestEnsureADOFixtureResolvesOldestOpenFixture(t *testing.T) {
	t.Parallel()
	board := newFakeADOBoard(t, 2000)
	board.add(5, ADOFixtureTitle, "Closed")
	board.add(9, "another tagged item", "Active")
	board.add(1776, ADOFixtureTitle, "Active")
	board.add(1800, ADOFixtureTitle, "Active")
	srv := httptest.NewServer(board)
	defer srv.Close()

	got, err := EnsureADOFixture(context.Background(), ensureFixtureConfig(srv, true))
	if err != nil {
		t.Fatal(err)
	}
	if got != (ADOFixtureResolution{WorkItem: "1776"}) || board.createCount() != 0 {
		t.Fatalf("resolution = %+v after %d creates, want the oldest open fixture #1776 and no create", got, board.createCount())
	}
}

// TestEnsureADOFixtureMissingFixture covers a closed (or deleted) fixture:
// without provisioning the error names the recovery, and with it a new open
// fixture is created that a refresh then records.
func TestEnsureADOFixtureMissingFixture(t *testing.T) {
	t.Parallel()
	board := newFakeADOBoard(t, 1812)
	board.add(1776, ADOFixtureTitle, "Closed")
	srv := httptest.NewServer(board)
	defer srv.Close()

	_, err := EnsureADOFixture(context.Background(), ensureFixtureConfig(srv, false))
	if !errors.Is(err, ErrADOFixtureMissing) || !strings.Contains(err.Error(), "-provision-fixture") {
		t.Fatalf("EnsureADOFixture() error = %v, want ErrADOFixtureMissing naming -provision-fixture", err)
	}
	if board.createCount() != 0 {
		t.Fatal("EnsureADOFixture created a fixture without Provision")
	}

	got, err := EnsureADOFixture(context.Background(), ensureFixtureConfig(srv, true))
	if err != nil {
		t.Fatal(err)
	}
	if got != (ADOFixtureResolution{WorkItem: "1812", Created: true}) || board.createCount() != 1 {
		t.Fatalf("resolution = %+v after %d creates, want created #1812", got, board.createCount())
	}
	again, err := EnsureADOFixture(context.Background(), ensureFixtureConfig(srv, true))
	if err != nil || again != (ADOFixtureResolution{WorkItem: "1812"}) || board.createCount() != 1 {
		t.Fatalf("second ensure = %+v, %v after %d creates; want the recreated fixture reused", again, err, board.createCount())
	}
	fixture, err := RefreshADO(context.Background(), ADORefreshConfig{
		OrganizationURL: srv.URL + "/acme",
		Project:         "Widgets",
		WorkItem:        got.WorkItem,
		Token:           "ado-pat",
	})
	if err != nil {
		t.Fatalf("refresh of the recreated fixture: %v", err)
	}
	if err := CheckContract(context.Background(), fixture); err != nil {
		t.Fatalf("recreated fixture fails the contract: %v", err)
	}
}

// TestRefreshADONormalizesTheFixtureWorkItemNumber: a recreated fixture has a
// new number, which must not read as drift against the baseline.
func TestRefreshADONormalizesTheFixtureWorkItemNumber(t *testing.T) {
	t.Parallel()
	refresh := func(id int) Fixture {
		t.Helper()
		board := newFakeADOBoard(t, 0)
		board.add(id, ADOFixtureTitle, "Active")
		srv := httptest.NewServer(board)
		defer srv.Close()
		fixture, err := RefreshADO(context.Background(), ADORefreshConfig{
			OrganizationURL: srv.URL + "/acme",
			Project:         "Widgets",
			WorkItem:        strconv.Itoa(id),
			Token:           "ado-pat",
		})
		if err != nil {
			t.Fatal(err)
		}
		return fixture
	}
	original, recreated := refresh(1776), refresh(17760)
	if err := CheckDrift(original, recreated); err != nil {
		raw, _ := canonical(recreated)
		t.Fatalf("recreated fixture drifted: %v\n%s", err, raw)
	}
	raw, err := canonical(recreated)
	if err != nil {
		t.Fatal(err)
	}
	if recreated.Issue != normalizedADOWorkItem || bytes.Contains(raw, []byte("1776")) {
		t.Fatalf("fixture kept the live work-item number:\n%s", raw)
	}
	if err := CheckContract(context.Background(), recreated); err != nil {
		t.Fatalf("normalized fixture fails the contract: %v", err)
	}
}
