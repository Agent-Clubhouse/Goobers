package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// adoBlockerFixture serves one successor (id 100) whose predecessor links
// point at the fixture's other items, the stock Agile state categories per
// type, and workitemsbatch hydration. An id missing from items answers 404,
// which workitemsbatch returns as an omitted (null) entry.
type adoBlockerFixture struct {
	mu          sync.Mutex
	items       map[int]map[string]interface{}
	statesReads map[string]int
}

// adoAgileStates is the stock Agile process: a Bug's Resolved state is in the
// Resolved category, a User Story's Resolved state is in InProgress.
var adoAgileStates = map[string][]map[string]string{
	"bug": {
		{"name": "New", "category": "Proposed"},
		{"name": "Active", "category": "InProgress"},
		{"name": "Resolved", "category": "Resolved"},
		{"name": "Closed", "category": "Completed"},
		{"name": "Removed", "category": "Removed"},
	},
	"user story": {
		{"name": "New", "category": "Proposed"},
		{"name": "Active", "category": "InProgress"},
		{"name": "Resolved", "category": "InProgress"},
		{"name": "Closed", "category": "Completed"},
		{"name": "Removed", "category": "Removed"},
	},
}

func adoBlockerItem(id int, itemType, state string) map[string]interface{} {
	return map[string]interface{}{
		"id":  id,
		"rev": 1,
		"fields": map[string]interface{}{
			"System.WorkItemType": itemType,
			"System.State":        state,
			"System.Title":        "item " + strconv.Itoa(id),
		},
	}
}

func adoPredecessorRelation(id int) map[string]interface{} {
	return map[string]interface{}{
		"rel":        adoPredecessorRel,
		"url":        "https://dev.azure.com/org/_apis/wit/workItems/" + strconv.Itoa(id),
		"attributes": map[string]interface{}{"name": "Predecessor"},
	}
}

// newADOBlockerFixture builds the successor linked to each predecessor id,
// plus the given predecessor items.
func newADOBlockerFixture(t *testing.T, predecessorIDs []int, predecessors ...map[string]interface{}) (*adoBlockerFixture, *ADOProvider) {
	t.Helper()
	successor := adoBlockerItem(100, "User Story", "New")
	relations := make([]map[string]interface{}, 0, len(predecessorIDs)+1)
	relations = append(relations, map[string]interface{}{
		"rel": "System.LinkTypes.Hierarchy-Reverse",
		"url": "https://dev.azure.com/org/_apis/wit/workItems/7",
	})
	for _, id := range predecessorIDs {
		relations = append(relations, adoPredecessorRelation(id))
	}
	successor["relations"] = relations
	fixture := &adoBlockerFixture{items: map[int]map[string]interface{}{100: successor}, statesReads: map[string]int{}}
	for _, item := range predecessors {
		fixture.items[item["id"].(int)] = item
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/org/", fixture.serve(t))
	server := httptest.NewServer(withADOTestWorkItemsBatch(t, mux))
	t.Cleanup(server.Close)
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	return fixture, provider
}

func (f *adoBlockerFixture) serve(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// org/{project}/_apis/wit/{resource}/...
		if len(parts) < 6 || parts[2] != "_apis" || parts[3] != "wit" {
			http.NotFound(w, r)
			return
		}
		project := parts[1]
		switch parts[4] {
		case "workitems":
			id, err := strconv.Atoi(parts[5])
			f.mu.Lock()
			item, ok := f.items[id]
			f.mu.Unlock()
			if err != nil || !ok {
				http.NotFound(w, r)
				return
			}
			writeJSON(t, w, item)
		case "workitemtypes":
			itemType := strings.ToLower(parts[5])
			states, ok := adoAgileStates[itemType]
			if !ok {
				http.NotFound(w, r)
				return
			}
			f.mu.Lock()
			f.statesReads[project+"/"+itemType]++
			f.mu.Unlock()
			writeJSON(t, w, map[string]interface{}{"value": states})
		default:
			http.NotFound(w, r)
		}
	}
}

var adoBlockerRepo = RepositoryRef{Provider: ProviderADO, Project: "project", Name: "repo"}

// TestADOPredecessorStateDecidesBlocking pins ADO-N32: a predecessor blocks
// until its state is done. By default the Resolved, Completed and Removed
// categories are done, so a Resolved Bug no longer blocks, but a User Story
// whose Resolved state is in the InProgress category still does.
// backlog.doneStates overrides the default per category and per type.
func TestADOPredecessorStateDecidesBlocking(t *testing.T) {
	for _, tc := range []struct {
		name        string
		itemType    string
		state       string
		doneStates  *ADODoneStates
		wantBlocked bool
	}{
		{name: "closed bug", itemType: "Bug", state: "Closed"},
		{name: "active bug", itemType: "Bug", state: "Active", wantBlocked: true},
		{name: "new bug", itemType: "Bug", state: "New", wantBlocked: true},
		{name: "resolved bug by default", itemType: "Bug", state: "Resolved"},
		{name: "removed predecessor", itemType: "User Story", state: "Removed"},
		{name: "resolved user story is in progress", itemType: "User Story", state: "Resolved", wantBlocked: true},
		{
			name: "byType makes a resolved bug block", itemType: "Bug", state: "Resolved",
			doneStates: &ADODoneStates{ByType: map[string][]string{"Bug": {"Closed"}}}, wantBlocked: true,
		},
		{
			name: "byType type and state names ignore case", itemType: "Bug", state: "Closed",
			doneStates: &ADODoneStates{ByType: map[string][]string{"bug": {"closed"}}},
		},
		{
			name: "byType leaves other types on categories", itemType: "User Story", state: "Closed",
			doneStates: &ADODoneStates{ByType: map[string][]string{"Bug": {"Closed"}}},
		},
		{
			name: "categories override the default", itemType: "Bug", state: "Resolved",
			doneStates: &ADODoneStates{Categories: []string{"Completed", "Removed"}}, wantBlocked: true,
		},
		{
			name: "categories can admit in-progress states", itemType: "User Story", state: "Resolved",
			doneStates: &ADODoneStates{Categories: []string{"InProgress", "Completed"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, provider := newADOBlockerFixture(t, []int{41}, adoBlockerItem(41, tc.itemType, tc.state))
			if tc.doneStates != nil {
				WithADODoneStates(*tc.doneStates)(provider)
			}
			blocked, err := provider.HasOpenWorkItemBlocker(context.Background(), adoBlockerRepo, "100")
			if err != nil {
				t.Fatalf("HasOpenWorkItemBlocker: %v", err)
			}
			if blocked != tc.wantBlocked {
				t.Fatalf("blocked = %v, want %v", blocked, tc.wantBlocked)
			}
		})
	}
}

// TestADOListWorkItemBlockersNamesOnlyOpenPredecessors pins the exclusion
// reason's input: only predecessors that still block are returned, each
// reported open, and a predecessor the batch omits still blocks.
func TestADOListWorkItemBlockersNamesOnlyOpenPredecessors(t *testing.T) {
	_, provider := newADOBlockerFixture(t, []int{41, 42, 43, 44, 42},
		adoBlockerItem(41, "Bug", "Closed"),
		adoBlockerItem(42, "Bug", "Active"),
		adoBlockerItem(43, "User Story", "Resolved"),
	)
	blockers, err := provider.ListWorkItemBlockers(context.Background(), adoBlockerRepo, "100")
	if err != nil {
		t.Fatalf("ListWorkItemBlockers: %v", err)
	}
	var ids []string
	for _, blocker := range blockers {
		if blocker.State != "open" {
			t.Errorf("blocker %s state = %q, want open", blocker.ID, blocker.State)
		}
		ids = append(ids, blocker.ID)
	}
	if got, want := strings.Join(ids, ","), "42,43,44"; got != want {
		t.Fatalf("blockers = %s, want %s (closed 41 left out, 44 omitted by the batch still blocks)", got, want)
	}
}

// TestADOHasOpenWorkItemBlockerWithoutPredecessors: an item with no
// predecessor links is not blocked and needs no batch call.
func TestADOHasOpenWorkItemBlockerWithoutPredecessors(t *testing.T) {
	_, provider := newADOBlockerFixture(t, nil)
	blocked, err := provider.HasOpenWorkItemBlocker(context.Background(), adoBlockerRepo, "100")
	if err != nil || blocked {
		t.Fatalf("HasOpenWorkItemBlocker = %v, %v; want false, nil", blocked, err)
	}
}

// TestADOPredecessorStatesReadForItsOwnProject: a cross-project predecessor's
// state categories come from its own project's process.
func TestADOPredecessorStatesReadForItsOwnProject(t *testing.T) {
	predecessor := adoBlockerItem(41, "Bug", "Closed")
	predecessor["fields"].(map[string]interface{})["System.TeamProject"] = "other-project"
	fixture, provider := newADOBlockerFixture(t, []int{41}, predecessor)
	blocked, err := provider.HasOpenWorkItemBlocker(context.Background(), adoBlockerRepo, "100")
	if err != nil || blocked {
		t.Fatalf("HasOpenWorkItemBlocker = %v, %v; want false, nil", blocked, err)
	}
	if fixture.statesReads["other-project/bug"] != 1 || fixture.statesReads["project/bug"] != 0 {
		t.Fatalf("state reads = %v, want one read for other-project/bug only", fixture.statesReads)
	}
}

// TestADOPredecessorUnknownStateFailsClosed: a state the type does not
// define is an error, which backlog-query turns into an exclusion.
func TestADOPredecessorUnknownStateFailsClosed(t *testing.T) {
	_, provider := newADOBlockerFixture(t, []int{41}, adoBlockerItem(41, "Bug", "Triaged"))
	if _, err := provider.HasOpenWorkItemBlocker(context.Background(), adoBlockerRepo, "100"); err == nil {
		t.Fatal("HasOpenWorkItemBlocker succeeded for an unknown predecessor state, want an error")
	}
}

// TestADODeclaresBacklogBlockers: ADO declares backlog.blockers, so the
// Dispatcher reaches the real check instead of failing closed.
func TestADODeclaresBacklogBlockers(t *testing.T) {
	_, provider := newADOBlockerFixture(t, []int{41}, adoBlockerItem(41, "Bug", "Closed"))
	if !provider.Capabilities().Has(CapBacklogBlockers) {
		t.Fatal("ADO does not declare backlog.blockers")
	}
	blocked, err := NewDispatcher(provider).HasOpenWorkItemBlocker(context.Background(), adoBlockerRepo, "100")
	if err != nil || blocked {
		t.Fatalf("Dispatcher.HasOpenWorkItemBlocker = %v, %v; want false, nil", blocked, err)
	}
}
