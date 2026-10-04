package workbenchgraph

import (
	"context"
	"crypto/sha1" // Native Git object fixture.
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
	"github.com/goobers/goobers/providers"
)

const objectiveID = "obj-00000000-0000-0000-0000-000000000001"
const edgeID = "edge-00000000-0000-0000-0000-000000000001"

var commit = strings.Repeat("a", 40)

func sourceSet(t *testing.T, paths ...string) workbench.SourceSet {
	t.Helper()
	target := apiv1.InteractiveRepositoryIdentity{Provider: "github", Owner: "org", Name: "repo"}
	g := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "team"}, Spec: apiv1.GaggleSpec{Project: apiv1.RepoRef{Provider: "github", Owner: "org", Name: "repo", Branch: "main"}, Backlog: apiv1.BacklogRef{Provider: "github", Project: "org/repo"}, Workbench: &apiv1.GaggleWorkbench{SchemaVersion: "sources/v1", RelationshipManifest: "links", Sources: []apiv1.WorkbenchSource{
		{Name: "items", Kind: "backlog"},
		{Name: "strategy", Kind: "documents", Repository: &target, Paths: paths},
		{Name: "links", Kind: "relationships", Repository: &target, Paths: []string{"links.yaml"}},
	}}}}
	set, err := workbench.BindSources(g)
	if err != nil {
		t.Fatal(err)
	}
	return set
}
func ref(binding, kind, id string) workbench.NodeRef {
	return workbench.NodeRef{GaggleID: "team", SourceBindingID: binding, Kind: kind, SourceID: id}
}
func nativeItem(id string) workbench.BacklogItem {
	return workbench.BacklogItem{Ref: ref("items", "work-item", id), Title: "Native item " + id, Revision: "rev1", State: "open", Type: "Issue", Locator: workbench.SourceLocator{ID: id}, RelationshipCoverage: workbench.RelationshipCoverage{Parents: "complete", Blockers: "complete", Milestones: "unsupported"}}
}
func backlog(t *testing.T, set workbench.SourceSet, items ...workbench.BacklogItem) BacklogWindow {
	t.Helper()
	target, err := workbench.SourceTargetDigest(set.Scope, set.Sources[0])
	if err != nil {
		t.Fatal(err)
	}
	return BacklogWindow{SourceBindingID: "items", Page: workbench.BacklogPage{Items: items, SourceTargetDigest: target, Exhausted: true, Candidates: len(items)}}
}
func documentFile(path string, objective bool) workbench.DocumentFileRead {
	file := workbench.DocumentFileRead{Path: path, Status: "available", Body: "source", Provenance: &workbench.SourceProvenance{Commit: commit, BlobID: strings.Repeat("b", 40), ContentDigest: strings.Repeat("c", 64)}}
	if objective {
		r := ref("strategy", "objective-document", objectiveID)
		file.Ref = &r
		file.Objective = &workbench.ObjectiveMetadata{SchemaVersion: "objectives/v1", ObjectiveID: objectiveID, Title: "Objective"}
	}
	return file
}
func documentPage(t *testing.T, set workbench.SourceSet, index, offset int, files ...workbench.DocumentFileRead) workbench.DocumentPage {
	t.Helper()
	source := set.Sources[index]
	target, err := workbench.SourceTargetDigest(set.Scope, source)
	if err != nil {
		t.Fatal(err)
	}
	return workbench.DocumentPage{SourceBindingID: source.Spec.Name, Repository: *source.Spec.Repository, Branch: source.Repository.Branch, Commit: commit, SourceTargetDigest: target, Files: files, StartOffset: offset, TotalPaths: len(source.Spec.Paths), Exhausted: offset+len(files) == len(source.Spec.Paths), Coverage: "partial"}
}
func manifestPage(t *testing.T, set workbench.SourceSet, edges ...workbench.Edge) workbench.DocumentPage {
	t.Helper()
	file := documentFile("links.yaml", false)
	file.Body = ""
	file.Manifest = &workbench.Manifest{SchemaVersion: "relationships/v1", Edges: edges}
	return documentPage(t, set, 2, 0, file)
}
func cloneSnapshot(t *testing.T, in Snapshot) Snapshot {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Snapshot
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGraphUsesOnlyExplicitRelationsAndAuthorizedTargetMembership(t *testing.T) {
	set := sourceSet(t, "objective.md", "plain.md")
	child, parent := nativeItem("2"), nativeItem("1")
	child.State = "closed"
	child.Relationships = []workbench.NativeRelationship{{Kind: "parent-of", Incoming: true, Target: workbench.NativeTarget{Kind: "work-item", StableID: "1", Locator: workbench.SourceLocator{ID: "1"}}}, {Kind: "blocked-by", Target: workbench.NativeTarget{Kind: "work-item", StableID: "private", Locator: workbench.SourceLocator{ID: "private"}}}}
	objective := documentFile("objective.md", true)
	objective.Objective.Edges = []workbench.Edge{{EdgeID: edgeID, Kind: "references", From: *objective.Ref, To: child.Ref}}
	snapshot := Snapshot{Sources: set, Backlogs: []BacklogWindow{backlog(t, set, child, parent)}, Documents: []workbench.DocumentPage{documentPage(t, set, 1, 0, objective), documentPage(t, set, 1, 1, documentFile("plain.md", false)), manifestPage(t, set)}}
	before := cloneSnapshot(t, snapshot)
	graph, err := Project(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, snapshot) {
		t.Fatal("projection mutated caller snapshots")
	}
	if len(graph.Nodes) != 3 || len(graph.Edges) != 3 || len(graph.Documents) != 3 || len(graph.Conflicts) != 0 || !graph.Partial {
		t.Fatalf("graph=%+v", graph)
	}
	for _, edge := range graph.Edges {
		switch edge.Kind {
		case "parent-of":
			if edge.From.Ref == nil || *edge.From.Ref != parent.Ref || !edge.From.Resolved || edge.To.Ref == nil || *edge.To.Ref != child.Ref || !edge.To.Resolved {
				t.Fatal("parent direction/membership lost", edge)
			}
		case "blocked-by":
			if edge.To.Ref != nil || edge.To.Native == nil || edge.To.Resolved {
				t.Fatal("unread project link became authorized", edge)
			}
		case "references":
			if !edge.From.Resolved || !edge.To.Resolved || edge.Origin != "authored" {
				t.Fatal(edge)
			}
		}
	}
	for _, coverage := range graph.Sources {
		if coverage.SourceBindingID == "items" && coverage.Consistency != "native-non-snapshot" {
			t.Fatal("native read promoted to snapshot")
		}
		if coverage.SourceBindingID == "strategy" && coverage.Status != "complete" {
			t.Fatal("coherent document windows not composed", coverage)
		}
	}
	// Closed state and ordinary Markdown never create contribution/progress edges.
	if graph.Nodes[0].Key == "" {
		t.Fatal("missing stable projection identity")
	}
	snapshot.Documents[0], snapshot.Documents[1] = snapshot.Documents[1], snapshot.Documents[0]
	snapshot.Backlogs[0].Page.Items[0], snapshot.Backlogs[0].Page.Items[1] = snapshot.Backlogs[0].Page.Items[1], snapshot.Backlogs[0].Page.Items[0]
	reordered, err := Project(snapshot)
	if err != nil || !reflect.DeepEqual(graph, reordered) {
		t.Fatal("scan order changed graph", err)
	}
}

func TestGraphMarksDuplicateObjectivesAndCompetingEdgeOwners(t *testing.T) {
	set := sourceSet(t, "one.md", "two.md")
	one, two := documentFile("one.md", true), documentFile("two.md", true)
	item := nativeItem("1")
	edge := workbench.Edge{EdgeID: edgeID, Kind: "references", From: *one.Ref, To: item.Ref}
	one.Objective.Edges = []workbench.Edge{edge}
	manifest := manifestPage(t, set, edge)
	graph, err := Project(Snapshot{Sources: set, Backlogs: []BacklogWindow{backlog(t, set, item)}, Documents: []workbench.DocumentPage{documentPage(t, set, 1, 0, one, two), manifest}})
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, conflict := range graph.Conflicts {
		kinds = append(kinds, conflict.Kind)
	}
	if !slices.Contains(kinds, "objective-location-conflict") || !slices.Contains(kinds, "edge-owner-conflict") || !slices.Contains(kinds, "edge-id-conflict") {
		t.Fatal("conflict hidden", graph.Conflicts)
	}
	for _, node := range graph.Nodes {
		if node.Key == one.Ref.Key() && (!node.Conflict || len(node.Observations) != 2) {
			t.Fatal("objective winner chosen", node)
		}
	}
	for _, projected := range graph.Edges {
		if !projected.Conflict || projected.From.Resolved {
			t.Fatal("conflict became usable edge", projected)
		}
	}
}

func TestGraphRefusesMixedRevisionsTargetsAndScope(t *testing.T) {
	set := sourceSet(t, "one.md", "two.md")
	input := Snapshot{Sources: set, Backlogs: []BacklogWindow{backlog(t, set, nativeItem("1"))}, Documents: []workbench.DocumentPage{documentPage(t, set, 1, 0, documentFile("one.md", false)), documentPage(t, set, 1, 1, documentFile("two.md", false)), manifestPage(t, set)}}
	for _, tc := range []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"mixed-commit", func(s *Snapshot) { s.Documents[1].Commit = strings.Repeat("d", 40) }},
		{"other-binding-same-repo-commit", func(s *Snapshot) { s.Documents[2].Commit = strings.Repeat("d", 40) }},
		{"target", func(s *Snapshot) { s.Documents[0].Repository.Name = "foreign" }},
		{"branch", func(s *Snapshot) { s.Documents[0].Branch = "foreign" }},
		{"path", func(s *Snapshot) { s.Documents[0].Files[0].Path = "secret.md" }},
		{"backlog-target", func(s *Snapshot) { s.Backlogs[0].Page.SourceTargetDigest = strings.Repeat("d", 64) }},
		{"gaggle", func(s *Snapshot) { s.Backlogs[0].Page.Items[0].Ref.GaggleID = "other" }},
		{"provenance", func(s *Snapshot) { s.Documents[0].Files[0].Provenance.Commit = strings.Repeat("d", 40) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := cloneSnapshot(t, input)
			tc.mutate(&changed)
			graph, err := Project(changed)
			if !errors.Is(err, ErrInvalidSnapshot) || len(graph.Nodes) > 0 {
				t.Fatalf("unsafe graph %+v %v", graph, err)
			}
		})
	}
}

func TestGraphPartialMissingFilesAndUnselectedManifest(t *testing.T) {
	set := sourceSet(t, "one.md", "two.md")
	set.ManifestOwner = nil
	manifest := manifestPage(t, set, workbench.Edge{EdgeID: edgeID, Kind: "references", From: ref("items", "work-item", "1"), To: ref("items", "work-item", "2")})
	missing := workbench.DocumentFileRead{Path: "one.md", Status: "unavailable"}
	graph, err := Project(Snapshot{Sources: set, Documents: []workbench.DocumentPage{documentPage(t, set, 1, 0, missing), manifest}})
	if err != nil || !graph.Partial || len(graph.Nodes) != 0 || len(graph.Edges) != 0 {
		t.Fatalf("%+v %v", graph, err)
	}
	for _, c := range graph.Sources {
		if c.SourceBindingID == "strategy" && (!slices.Contains(c.Reasons, "paths-not-read") || !slices.Contains(c.Reasons, "source-omissions")) {
			t.Fatal(c)
		}
		if c.SourceBindingID == "items" && c.Status != "not-read" {
			t.Fatal(c)
		}
	}
}

func TestGraphBoundsAndNativeRevisionConflicts(t *testing.T) {
	set := sourceSet(t, "one.md")
	first := backlog(t, set, nativeItem("1"))
	second := backlog(t, set, nativeItem("1"))
	second.Page.Items[0].Revision = "rev2"
	graph, err := Project(Snapshot{Sources: set, Backlogs: []BacklogWindow{first, second}})
	if err != nil || len(graph.Nodes) != 1 || !graph.Nodes[0].Conflict || len(graph.Nodes[0].Observations) != 2 {
		t.Fatalf("%+v %v", graph, err)
	}
	if _, err = Project(Snapshot{Sources: set, Backlogs: make([]BacklogWindow, MaxPages+1)}); !errors.Is(err, ErrProjectionBound) {
		t.Fatal("page bound missing", err)
	}
	oversized := documentPage(t, set, 1, 0, documentFile("one.md", false))
	oversized.Files[0].Body = strings.Repeat("x", workbench.MaxDocumentPageBytes)
	if _, err = Project(Snapshot{Sources: set, Documents: []workbench.DocumentPage{oversized}}); !errors.Is(err, ErrProjectionBound) {
		t.Fatal("byte bound missing", err)
	}
}

// This fixture uses the actual repository adapter and frontmatter parser before
// projection; graph tests do not rely only on hand-assembled document metadata.
type sourceReader struct{ source string }

func (sourceReader) Kind() providers.ProviderKind { return providers.ProviderGitHub }
func (sourceReader) ReadSourceBranch(context.Context, providers.RepositoryRef, string) (string, error) {
	return commit, nil
}
func (s sourceReader) ReadRepositorySource(_ context.Context, _ providers.RepositoryRef, path, pin string) (providers.RepositorySourceFile, error) {
	h := sha1.New() //nolint:gosec // Native Git object fixture.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(s.source))
	_, _ = h.Write([]byte(s.source))
	return providers.RepositorySourceFile{Commit: pin, Path: path, BlobID: hex.EncodeToString(h.Sum(nil)), Content: []byte(s.source)}, nil
}
func TestGraphComposesVerifiedRepositoryAdapter(t *testing.T) {
	set := sourceSet(t, "objective.md")
	raw := "---\ngoobers:\n  schemaVersion: objectives/v1\n  objectiveId: " + objectiveID + "\n  title: Parsed source\n---\n# context\n"
	reader, err := workbenchprovider.NewRepositoryReader(set.Scope, set.Sources[1], sourceReader{source: raw})
	if err != nil {
		t.Fatal(err)
	}
	page, err := reader.Page(context.Background(), workbench.DocumentPageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := Project(Snapshot{Sources: set, Documents: []workbench.DocumentPage{page}})
	if err != nil || len(graph.Nodes) != 1 || graph.Nodes[0].Observations[0].Title != "Parsed source" || graph.Nodes[0].Observations[0].Provenance.ContentDigest == "" {
		t.Fatalf("%+v %v", graph, err)
	}
}

func TestGraphRefusesNodeOverflowAndFlagsNativeManifestOwnership(t *testing.T) {
	set := sourceSet(t, "one.md")
	item := nativeItem("2")
	parent := nativeItem("1")
	item.Relationships = []workbench.NativeRelationship{{Kind: "parent-of", Incoming: true, Target: workbench.NativeTarget{Kind: "work-item", StableID: "1", Locator: workbench.SourceLocator{ID: "1"}}}}
	explicit := workbench.Edge{EdgeID: edgeID, Kind: "parent-of", From: parent.Ref, To: item.Ref}
	graph, err := Project(Snapshot{Sources: set, Backlogs: []BacklogWindow{backlog(t, set, parent, item)}, Documents: []workbench.DocumentPage{manifestPage(t, set, explicit)}})
	if err != nil || len(graph.Edges) != 2 {
		t.Fatalf("%+v %v", graph, err)
	}
	for _, edge := range graph.Edges {
		if !edge.Conflict {
			t.Fatal("native owner overridden by manifest", edge)
		}
		if edge.Origin == "native" && edge.EdgeID != "" {
			t.Fatal("native relation acquired invented source identity")
		}
	}
	input := Snapshot{Sources: set}
	for first := 1; first <= MaxNodes+1; first += workbench.MaxBacklogPageItems {
		var items []workbench.BacklogItem
		for id := first; id < first+workbench.MaxBacklogPageItems && id <= MaxNodes+1; id++ {
			items = append(items, nativeItem(fmt.Sprint(id)))
		}
		input.Backlogs = append(input.Backlogs, backlog(t, set, items...))
	}
	if _, err = Project(input); !errors.Is(err, ErrProjectionBound) {
		t.Fatal("node bound missing", err)
	}
}
