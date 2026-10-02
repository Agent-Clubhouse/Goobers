package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

// adoAncestryServer serves workitemsbatch from a fixed item table, recording
// each batch's ids. An id absent from the table comes back null, as
// errorPolicy=Omit returns a deleted or unreadable item. status, when set,
// fails every batch with that HTTP status.
type adoAncestryServer struct {
	mu      sync.Mutex
	items   map[int]map[string]interface{}
	batches [][]int
	status  int
}

func adoAncestryItem(id int, project, itemType string, parent int, fields map[string]interface{}) map[string]interface{} {
	all := map[string]interface{}{
		"System.TeamProject":  project,
		"System.WorkItemType": itemType,
		"System.Title":        itemType + " " + strconv.Itoa(id),
		"System.State":        "In Planning",
	}
	for name, value := range fields {
		all[name] = value
	}
	relations := []map[string]interface{}{
		{"rel": "System.LinkTypes.Related", "url": "https://ado.example/_apis/wit/workItems/999"},
	}
	if parent > 0 {
		relations = append(relations, map[string]interface{}{"rel": "System.LinkTypes.Hierarchy-Reverse", "url": "https://ado.example/_apis/wit/workItems/" + strconv.Itoa(parent)})
	}
	return map[string]interface{}{"id": id, "rev": 3, "url": "https://ado.example/_apis/wit/workItems/" + strconv.Itoa(id), "fields": all, "relations": relations}
}

func newADOAncestryServer(t *testing.T, fake *adoAncestryServer) *ADOProvider {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/wit/workitemsbatch", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		body, ok := decodeADOTestBatchRequest(t, w, r)
		if !ok {
			return
		}
		fake.mu.Lock()
		fake.batches = append(fake.batches, append([]int(nil), body.IDs...))
		status := fake.status
		fake.mu.Unlock()
		if status != 0 {
			http.Error(w, "denied", status)
			return
		}
		values := make([]interface{}, 0, len(body.IDs))
		for _, id := range body.IDs {
			item, ok := fake.items[id]
			if !ok {
				values = append(values, nil)
				continue
			}
			values = append(values, item)
		}
		writeJSON(t, w, map[string]interface{}{"count": len(values), "value": values})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected ADO request %s %s: ancestry must read only through workitemsbatch", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
}

func adoAncestryRoot(provider *ADOProvider, id string, parent int) WorkItemNode {
	item := mapADOWorkItemState(adoWorkItem{
		ID:     mustAtoi(id),
		Fields: adoAncestryItem(mustAtoi(id), "project", "Task", parent, nil)["fields"].(map[string]interface{}),
		Relations: []adoRelation{
			{Rel: "System.LinkTypes.Hierarchy-Reverse", URL: "https://ado.example/_apis/wit/workItems/" + strconv.Itoa(parent)},
		},
	}, "open", "")
	return provider.AncestryRoot(RepositoryRef{Project: "project"}, item)
}

func mustAtoi(value string) int {
	n, err := strconv.Atoi(value)
	if err != nil {
		panic(err)
	}
	return n
}

// TestADOAncestryWalksCustomProcessChainOneBatchPerLevel: Task 945 ->
// Feature 900 -> custom "Initiative" 800 -> Epic 700. Each level is one
// workitemsbatch call; the custom type maps with its native state and no
// state-category read; the intent fields come through; the Related link is
// never treated as a parent; the depth bound stops the walk before 700 is read.
func TestADOAncestryWalksCustomProcessChainOneBatchPerLevel(t *testing.T) {
	fake := &adoAncestryServer{items: map[int]map[string]interface{}{
		900: adoAncestryItem(900, "project", "Feature", 800, map[string]interface{}{
			"System.Description":                       "<p>Feature intent</p>",
			"Microsoft.VSTS.Common.AcceptanceCriteria": "<ul><li>Criterion</li></ul>",
		}),
		800: adoAncestryItem(800, "project", "Initiative", 700, map[string]interface{}{"System.Description": "Initiative intent"}),
		700: adoAncestryItem(700, "project", "Epic", 0, nil),
	}}
	provider := newADOAncestryServer(t, fake)
	root := adoAncestryRoot(provider, "945", 900)
	if root.ParentID != "900" || root.Key() != "ado:project:945" {
		t.Fatalf("root = %+v, want parent 900 from the Hierarchy-Reverse relation", root)
	}

	got, err := TraverseWorkItemAncestry(context.Background(), provider, RepositoryRef{Project: "project"}, []WorkItemNode{root},
		AncestryOptions{MaxDepth: 2, MaxItems: 10, CrossProject: AncestryCrossProjectDeny})
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]int{{900}, {800}}; !reflect.DeepEqual(fake.batches, want) {
		t.Fatalf("batches = %v, want %v", fake.batches, want)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %+v, want Feature and Initiative", got.Items)
	}
	feature, initiative := got.Items[0], got.Items[1]
	wantFields := []WorkItemField{
		{Name: "System.Description", Value: "<p>Feature intent</p>"},
		{Name: "Microsoft.VSTS.Common.AcceptanceCriteria", Value: "<ul><li>Criterion</li></ul>"},
	}
	if feature.Type != "Feature" || feature.State != "In Planning" || !reflect.DeepEqual(feature.Fields, wantFields) {
		t.Fatalf("feature = %+v", feature)
	}
	if initiative.Type != "Initiative" || initiative.Depth != 2 || initiative.ParentID != "700" {
		t.Fatalf("initiative = %+v, want the custom type at depth 2", initiative)
	}
	if want := []string{"ado:project:800>700@max-depth"}; !reflect.DeepEqual(omissionReasons(got), want) {
		t.Fatalf("omissions = %v, want %v", omissionReasons(got), want)
	}
}

func TestADOAncestryOmittedParentIsNotFound(t *testing.T) {
	provider := newADOAncestryServer(t, &adoAncestryServer{items: map[int]map[string]interface{}{}})
	got, err := TraverseWorkItemAncestry(context.Background(), provider, RepositoryRef{Project: "project"},
		[]WorkItemNode{adoAncestryRoot(provider, "945", 900)}, AncestryOptions{MaxDepth: 3, MaxItems: 10})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ado:project:945>900@not-found"}; !reflect.DeepEqual(omissionReasons(got), want) {
		t.Fatalf("omissions = %v, want %v", omissionReasons(got), want)
	}
}

func TestADOAncestryPermissionDeniedDegrades(t *testing.T) {
	provider := newADOAncestryServer(t, &adoAncestryServer{status: http.StatusForbidden})
	got, err := TraverseWorkItemAncestry(context.Background(), provider, RepositoryRef{Project: "project"},
		[]WorkItemNode{adoAncestryRoot(provider, "945", 900)}, AncestryOptions{MaxDepth: 3, MaxItems: 10})
	if err != nil {
		t.Fatalf("a denied parent read must not fail the walk: %v", err)
	}
	if want := []string{"ado:project:945>900@access-denied"}; !reflect.DeepEqual(omissionReasons(got), want) || got.Status != AncestryIncomplete {
		t.Fatalf("omissions = %v (%s), want %v incomplete", omissionReasons(got), got.Status, want)
	}
}

func TestADOAncestryCrossProjectParentDeniedByDefault(t *testing.T) {
	fake := &adoAncestryServer{items: map[int]map[string]interface{}{
		900: adoAncestryItem(900, "elsewhere", "Feature", 0, map[string]interface{}{"System.Description": "secret"}),
	}}
	provider := newADOAncestryServer(t, fake)
	got, err := TraverseWorkItemAncestry(context.Background(), provider, RepositoryRef{Project: "project"},
		[]WorkItemNode{adoAncestryRoot(provider, "945", 900)}, AncestryOptions{MaxDepth: 3, MaxItems: 10, CrossProject: AncestryCrossProjectDeny})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 0 || len(got.Omissions) != 1 || got.Omissions[0].Reason != AncestryOmitCrossProject {
		t.Fatalf("ancestry = %+v, want the cross-project parent omitted", got)
	}
}

func TestADOAncestryCycleStopsWithoutRereading(t *testing.T) {
	fake := &adoAncestryServer{items: map[int]map[string]interface{}{
		900: adoAncestryItem(900, "project", "Feature", 945, nil),
	}}
	provider := newADOAncestryServer(t, fake)
	got, err := TraverseWorkItemAncestry(context.Background(), provider, RepositoryRef{Project: "project"},
		[]WorkItemNode{adoAncestryRoot(provider, "945", 900)}, AncestryOptions{MaxDepth: 5, MaxItems: 10})
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]int{{900}}; !reflect.DeepEqual(fake.batches, want) {
		t.Fatalf("batches = %v, want %v", fake.batches, want)
	}
	if want := []string{"ado:project:900>945@cycle"}; !reflect.DeepEqual(omissionReasons(got), want) {
		t.Fatalf("omissions = %v, want %v", omissionReasons(got), want)
	}
}

// A child whose parent id is not a positive integer is a failed read that
// never reaches the batch; children sharing a parent put it in the batch
// once; and a parent without System.TeamProject is placed in the provider's
// project.
func TestADOAncestryReadsEachValidParentOnce(t *testing.T) {
	parent := adoAncestryItem(900, "ignored", "Feature", 0, nil)
	delete(parent["fields"].(map[string]interface{}), "System.TeamProject")
	fake := &adoAncestryServer{items: map[int]map[string]interface{}{900: parent}}
	provider := newADOAncestryServer(t, fake)
	children := []WorkItemNode{
		{Provider: ProviderADO, Project: "project", ID: "1", ParentID: "not-a-number"},
		{Provider: ProviderADO, Project: "project", ID: "2", ParentID: "900"},
		{Provider: ProviderADO, Project: "project", ID: "3", ParentID: "0"},
		{Provider: ProviderADO, Project: "project", ID: "4", ParentID: "900"},
	}
	reads, err := provider.ReadWorkItemParents(context.Background(), RepositoryRef{Project: "project"}, children, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]int{{900}}; !reflect.DeepEqual(fake.batches, want) {
		t.Fatalf("batches = %v, want %v", fake.batches, want)
	}
	for _, i := range []int{0, 2} {
		if r := reads[i]; r.Omission != AncestryOmitReadFailed || r.Detail != "invalid parent id" || r.Parent != nil || r.ParentID != children[i].ParentID {
			t.Errorf("read %d = %+v, want a failed read of the invalid parent id", i, r)
		}
	}
	for _, i := range []int{1, 3} {
		r := reads[i]
		if r.Omission != "" || r.Parent == nil || r.Parent.Key() != "ado:project:900" || r.Parent.Type != "Feature" {
			t.Errorf("read %d = %+v, want Feature 900 placed in the provider's project", i, r)
		}
	}
}

// A walk whose context has ended returns that error rather than recording
// every parent as an omission.
func TestADOAncestryCancelledContextIsAnError(t *testing.T) {
	provider := newADOAncestryServer(t, &adoAncestryServer{items: map[int]map[string]interface{}{}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reads, err := provider.ReadWorkItemParents(ctx, RepositoryRef{Project: "project"},
		[]WorkItemNode{{Provider: ProviderADO, Project: "project", ID: "1", ParentID: "900"}}, nil)
	if !errors.Is(err, context.Canceled) || reads != nil {
		t.Fatalf("ReadWorkItemParents = %+v, %v; want the cancellation", reads, err)
	}
}
