package providers

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// fakeParentReader is an in-memory WorkItemParentReader over a parent graph
// keyed by qualified key. Nodes report their parent inline (like Azure
// DevOps) unless unknownParents is set (like GitHub).
type fakeParentReader struct {
	nodes          map[string]WorkItemNode
	parentOf       map[string]string // child key -> parent key
	failures       map[string]string // parent key -> omission reason
	unknownParents bool
	calls          [][]string
}

func (f *fakeParentReader) AncestryRoot(_ RepositoryRef, item WorkItem) WorkItemNode {
	return f.withHint(f.nodes["ado:P:"+item.ID])
}

func (f *fakeParentReader) withHint(node WorkItemNode) WorkItemNode {
	if f.unknownParents {
		node.ParentUnknown = true
		return node
	}
	if parent, ok := f.parentOf[node.Key()]; ok {
		node.ParentID = parent[strings.LastIndex(parent, ":")+1:]
	}
	return node
}

func (f *fakeParentReader) ReadWorkItemParents(_ context.Context, _ RepositoryRef, children []WorkItemNode, fields []string) ([]WorkItemParentRead, error) {
	keys := make([]string, len(children))
	reads := make([]WorkItemParentRead, len(children))
	for i, child := range children {
		keys[i] = child.Key()
		parentKey, ok := f.parentOf[child.Key()]
		if !ok {
			continue
		}
		if reason, failed := f.failures[parentKey]; failed {
			reads[i] = WorkItemParentRead{ParentID: parentKey, Omission: reason}
			continue
		}
		parent := f.withHint(f.nodes[parentKey])
		reads[i] = WorkItemParentRead{Parent: &parent, ParentID: parent.ID}
	}
	f.calls = append(f.calls, keys)
	return reads, nil
}

func node(project, id, itemType string) WorkItemNode {
	return WorkItemNode{Provider: ProviderADO, Project: project, ID: id, Type: itemType, Title: itemType + " " + id, State: "Active", Integrity: "unapproved"}
}

// chainReader builds Task 1 -> Feature 2 -> Epic 3 -> Initiative 4 -> Theme 5
// in project P.
func chainReader() *fakeParentReader {
	return &fakeParentReader{
		nodes: map[string]WorkItemNode{
			"ado:P:1": node("P", "1", "Task"),
			"ado:P:2": node("P", "2", "Feature"),
			"ado:P:3": node("P", "3", "Epic"),
			"ado:P:4": node("P", "4", "Initiative"),
			"ado:P:5": node("P", "5", "Theme"),
		},
		parentOf: map[string]string{"ado:P:1": "ado:P:2", "ado:P:2": "ado:P:3", "ado:P:3": "ado:P:4", "ado:P:4": "ado:P:5"},
	}
}

func traverse(t *testing.T, reader *fakeParentReader, opts AncestryOptions, rootIDs ...string) WorkItemAncestry {
	t.Helper()
	roots := make([]WorkItemNode, len(rootIDs))
	for i, id := range rootIDs {
		roots[i] = reader.AncestryRoot(RepositoryRef{}, WorkItem{ID: id})
	}
	got, err := TraverseWorkItemAncestry(context.Background(), reader, RepositoryRef{}, roots, opts)
	if err != nil {
		t.Fatalf("TraverseWorkItemAncestry: %v", err)
	}
	return got
}

func ancestorKeys(ancestry WorkItemAncestry) []string {
	keys := make([]string, len(ancestry.Items))
	for i, item := range ancestry.Items {
		keys[i] = item.Key()
	}
	return keys
}

func omissionReasons(ancestry WorkItemAncestry) []string {
	reasons := make([]string, len(ancestry.Omissions))
	for i, omission := range ancestry.Omissions {
		reasons[i] = omission.Child + ">" + omission.Parent + "@" + omission.Reason
	}
	return reasons
}

func TestAncestryMaxDepthStopsBeforeReadingDeeperParents(t *testing.T) {
	reader := chainReader()
	got := traverse(t, reader, AncestryOptions{MaxDepth: 2, MaxItems: 10}, "1")
	if want := []string{"ado:P:2", "ado:P:3"}; !reflect.DeepEqual(ancestorKeys(got), want) {
		t.Fatalf("items = %v, want %v", ancestorKeys(got), want)
	}
	if want := []string{"ado:P:3>4@max-depth"}; !reflect.DeepEqual(omissionReasons(got), want) {
		t.Fatalf("omissions = %v, want %v", omissionReasons(got), want)
	}
	if got.Status != AncestryIncomplete || len(reader.calls) != 2 {
		t.Fatalf("status = %q after %d reads, want incomplete after 2", got.Status, len(reader.calls))
	}
	if got.Items[0].Depth != 1 || got.Items[1].Depth != 2 || !reflect.DeepEqual(got.Items[0].ParentOf, []string{"ado:P:1"}) {
		t.Fatalf("items = %+v, want depths 1,2 with the root as child", got.Items)
	}
}

func TestAncestryMaxItemsIsIndependentOfDepth(t *testing.T) {
	reader := chainReader()
	reader.nodes["ado:P:9"] = node("P", "9", "Task")
	reader.nodes["ado:P:8"] = node("P", "8", "Feature")
	reader.parentOf["ado:P:9"] = "ado:P:8"
	got := traverse(t, reader, AncestryOptions{MaxDepth: 10, MaxItems: 3}, "1", "9")
	if want := []string{"ado:P:2", "ado:P:8", "ado:P:3"}; !reflect.DeepEqual(ancestorKeys(got), want) {
		t.Fatalf("items = %v, want %v", ancestorKeys(got), want)
	}
	if want := []string{"ado:P:3>4@max-items"}; !reflect.DeepEqual(omissionReasons(got), want) {
		t.Fatalf("omissions = %v, want %v", omissionReasons(got), want)
	}
	if got.Status != AncestryIncomplete {
		t.Fatalf("status = %q, want incomplete", got.Status)
	}
}

func TestAncestryCompleteChainWithinBounds(t *testing.T) {
	got := traverse(t, chainReader(), AncestryOptions{MaxDepth: 5, MaxItems: 10}, "1")
	if len(got.Items) != 4 || got.Status != AncestryComplete || len(got.Omissions) != 0 {
		t.Fatalf("ancestry = %+v, want four items, complete", got)
	}
}

func TestAncestryCycleTerminatesAndIsReported(t *testing.T) {
	for name, unknown := range map[string]bool{"inline parent": false, "parent read separately": true} {
		t.Run(name, func(t *testing.T) {
			reader := chainReader()
			reader.unknownParents = unknown
			reader.parentOf["ado:P:3"] = "ado:P:1" // 1 -> 2 -> 3 -> 1
			got := traverse(t, reader, AncestryOptions{MaxDepth: 10, MaxItems: 10}, "1")
			if want := []string{"ado:P:2", "ado:P:3"}; !reflect.DeepEqual(ancestorKeys(got), want) {
				t.Fatalf("items = %v, want %v", ancestorKeys(got), want)
			}
			if len(got.Omissions) != 1 || got.Omissions[0].Reason != AncestryOmitCycle || got.Omissions[0].Child != "ado:P:3" {
				t.Fatalf("omissions = %+v, want one cycle at ado:P:3", got.Omissions)
			}
		})
	}
}

func TestAncestryMissingAndDeniedParentsAreRecordedNotFatal(t *testing.T) {
	for _, reason := range []string{AncestryOmitNotFound, AncestryOmitAccessDenied, AncestryOmitReadFailed} {
		t.Run(reason, func(t *testing.T) {
			reader := chainReader()
			reader.failures = map[string]string{"ado:P:3": reason}
			got := traverse(t, reader, AncestryOptions{MaxDepth: 5, MaxItems: 10}, "1")
			if want := []string{"ado:P:2"}; !reflect.DeepEqual(ancestorKeys(got), want) {
				t.Fatalf("items = %v, want %v", ancestorKeys(got), want)
			}
			if want := []string{"ado:P:2>ado:P:3@" + reason}; !reflect.DeepEqual(omissionReasons(got), want) {
				t.Fatalf("omissions = %v, want %v", omissionReasons(got), want)
			}
			if got.Status != AncestryIncomplete {
				t.Fatalf("status = %q, want incomplete", got.Status)
			}
		})
	}
}

// TestAncestryCustomTypesIncludeOrExclude: the include list names custom
// process types, matching case-insensitively; an excluded type is not
// serialized, but the walk continues through it and it is not a gap.
func TestAncestryCustomTypesIncludeOrExclude(t *testing.T) {
	got := traverse(t, chainReader(), AncestryOptions{MaxDepth: 5, MaxItems: 10, IncludeTypes: []string{"initiative", "Theme"}}, "1")
	if want := []string{"ado:P:4", "ado:P:5"}; !reflect.DeepEqual(ancestorKeys(got), want) {
		t.Fatalf("items = %v, want %v", ancestorKeys(got), want)
	}
	if got.Items[0].Depth != 3 || got.Items[0].Type != "Initiative" {
		t.Fatalf("first item = %+v, want the custom Initiative at depth 3", got.Items[0])
	}
	if got.Status != AncestryComplete || len(got.Omissions) != 2 || got.Omissions[0].Reason != AncestryOmitExcludedType {
		t.Fatalf("status %q omissions %+v, want complete with two excluded-type records", got.Status, got.Omissions)
	}
}

func TestAncestryCrossProjectPolicy(t *testing.T) {
	build := func() *fakeParentReader {
		reader := chainReader()
		reader.nodes["ado:Other:3"] = node("Other", "3", "Epic")
		reader.parentOf["ado:P:2"] = "ado:Other:3"
		delete(reader.parentOf, "ado:P:3")
		return reader
	}
	denied := traverse(t, build(), AncestryOptions{MaxDepth: 5, MaxItems: 10, CrossProject: AncestryCrossProjectDeny}, "1")
	if want := []string{"ado:P:2"}; !reflect.DeepEqual(ancestorKeys(denied), want) {
		t.Fatalf("deny items = %v, want %v", ancestorKeys(denied), want)
	}
	if want := []string{"ado:P:2>ado:Other:3@cross-project"}; !reflect.DeepEqual(omissionReasons(denied), want) {
		t.Fatalf("deny omissions = %v, want %v", omissionReasons(denied), want)
	}
	allowed := traverse(t, build(), AncestryOptions{MaxDepth: 5, MaxItems: 10, CrossProject: AncestryCrossProjectAllow}, "1")
	if want := []string{"ado:P:2", "ado:Other:3"}; !reflect.DeepEqual(ancestorKeys(allowed), want) || allowed.Status != AncestryComplete {
		t.Fatalf("allow items = %v (%s), want %v complete", ancestorKeys(allowed), allowed.Status, want)
	}
}

// TestAncestrySharedParentIsReadOnceAndOrderedDeterministically: two roots
// under one Feature produce one item naming both children, in stable order
// whatever order the roots arrive in.
func TestAncestrySharedParentIsReadOnceAndOrderedDeterministically(t *testing.T) {
	var first WorkItemAncestry
	for i, roots := range [][]string{{"1", "6"}, {"6", "1"}} {
		reader := chainReader()
		reader.nodes["ado:P:6"] = node("P", "6", "Task")
		reader.parentOf["ado:P:6"] = "ado:P:2"
		got := traverse(t, reader, AncestryOptions{MaxDepth: 1, MaxItems: 10}, roots...)
		if len(got.Items) != 1 || !reflect.DeepEqual(got.Items[0].ParentOf, []string{"ado:P:1", "ado:P:6"}) {
			t.Fatalf("items = %+v, want one Feature parenting both roots", got.Items)
		}
		if i == 0 {
			first = got
		} else if !reflect.DeepEqual(got, first) {
			t.Fatalf("root order changed the result:\n%+v\n%+v", got, first)
		}
	}
}

func TestBoundFieldsCutsOnRuneBoundary(t *testing.T) {
	got := boundFields([]WorkItemField{{Name: "d", Value: "ab€cd"}, {Name: "s", Value: "ok"}}, 4)
	if got[0].Value != "ab" || !got[0].Truncated || got[1].Value != "ok" || got[1].Truncated {
		t.Fatalf("bounded = %+v", got)
	}
}

func TestAncestryFieldsAreBounded(t *testing.T) {
	reader := chainReader()
	feature := reader.nodes["ado:P:2"]
	feature.Fields = []WorkItemField{{Name: "System.Description", Value: strings.Repeat("x", 100)}}
	reader.nodes["ado:P:2"] = feature
	got := traverse(t, reader, AncestryOptions{MaxDepth: 1, MaxItems: 10, MaxFieldBytes: 10}, "1")
	if field := got.Items[0].Fields[0]; len(field.Value) != 10 || !field.Truncated {
		t.Fatalf("field = %+v, want cut to 10 bytes and marked", field)
	}
}
