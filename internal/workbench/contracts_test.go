package workbench

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const objectiveID = "obj-8e1bc91c-94cb-4c5c-9412-d7e07e86d5da"
const edgeID = "edge-a72fdd9b-f947-449f-9dba-0fe2d9e6e587"

func contractScope() Scope {
	return Scope{GaggleID: "web", Bindings: map[string]bool{"backlog": true, "strategy": true, "links": true}}
}

func TestEdgeOwnersDoNotConflateHierarchyMembershipOrCompetingSources(t *testing.T) {
	scope, edge := contractScope(), contractEdge()
	edge.Kind, edge.To.Kind = "parent-of", "work-item"
	parent, err := ResolveOwner(scope, edge, Ownership{NativeField: "parent"})
	if err != nil || parent.Object != edge.To {
		t.Fatalf("native hierarchy owner: %+v %v", parent, err)
	}
	edge.Kind, edge.To.Kind = "milestone-member", "milestone"
	membership, err := ResolveOwner(scope, edge, Ownership{NativeField: "milestone"})
	if err != nil || membership.Object != edge.From {
		t.Fatalf("native milestone owner: %+v %v", membership, err)
	}
	one := OwnedEdge{Edge: contractEdge(), Owner: Owner{Kind: "manifest", SourceBindingID: "links", Path: "relationships.yaml"}}
	two := one
	two.Owner.Path = "other.yaml"
	if err := ValidateOwnership(scope, []OwnedEdge{one, one}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOwnership(scope, []OwnedEdge{one, two}); err == nil {
		t.Fatal("competing source locations accepted")
	}
	two = one
	two.Edge.EdgeID = "edge-11111111-1111-1111-1111-111111111111"
	if err := ValidateOwnership(scope, []OwnedEdge{one, two}); err == nil {
		t.Fatal("second identity for same edge accepted")
	}
}

func contractObjective() ObjectiveMetadata {
	return ObjectiveMetadata{SchemaVersion: "objectives/v1", ObjectiveID: objectiveID, Title: "Reliable payment processing"}
}

func contractEdge() Edge {
	return Edge{EdgeID: edgeID, Kind: "contributes-to", From: NodeRef{GaggleID: "web", SourceBindingID: "backlog", Kind: "work-item", SourceID: "provider-immutable-id"}, To: NodeRef{GaggleID: "web", SourceBindingID: "strategy", Kind: "objective-document", SourceID: objectiveID}}
}

func TestObjectiveProposalPreservesIdentityBodyAndUnrelatedMetadata(t *testing.T) {
	raw := []byte("---\r\n# owned by another system\r\nlayout: page\r\nauthors: [alice, bob]\r\n---\r\n# Body\r\n\r\nKeep exact $text and [links](../other.md).\r\n")
	proposed, err := ProposeObjectiveMetadata(raw, contractScope(), "strategy", contractObjective())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseDocument(proposed, contractScope(), "strategy")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Objective == nil || doc.Objective.ObjectiveID != objectiveID {
		t.Fatalf("identity lost: %+v", doc.Objective)
	}
	wantBody := []byte("# Body\r\n\r\nKeep exact $text and [links](../other.md).\r\n")
	if !bytes.Equal(doc.Body, wantBody) {
		t.Fatalf("body changed: %q", doc.Body)
	}
	var metadata map[string]any
	if err := doc.header.Decode(&metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["layout"] != "page" || !bytes.Contains(proposed, []byte("# owned by another system\r\n")) {
		t.Fatalf("unrelated frontmatter lost: %s", proposed)
	}
	authors, ok := metadata["authors"].([]any)
	if !ok || len(authors) != 2 || authors[1] != "bob" {
		t.Fatal(metadata)
	}
	changed := contractObjective()
	changed.Title = "Updated title"
	updated, err := ProposeObjectiveMetadata(proposed, contractScope(), "strategy", changed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(updated, wantBody) {
		t.Fatal("body changed during metadata edit")
	}
	changed.ObjectiveID = "obj-11111111-1111-1111-1111-111111111111"
	if _, err := ProposeObjectiveMetadata(updated, contractScope(), "strategy", changed); err == nil {
		t.Fatal("identity replaced")
	}
}

func TestMarkdownWithoutIdentityIsReferenceAndRenameRetainsObjective(t *testing.T) {
	for _, raw := range []string{"# Just prose\n", "---\n---\n# Empty metadata\n", "---\nlayout: page\n---\n# Reference\n"} {
		doc, err := ParseDocument([]byte(raw), contractScope(), "strategy")
		if err != nil || doc.Objective != nil {
			t.Fatalf("prose inferred objective: %v %+v", err, doc)
		}
	}
	value := LocatedObjective{Binding: "strategy", Path: "objectives/payments.md", Revision: "commit-a", Objective: contractObjective()}
	if err := ValidateObjectiveSet(contractScope(), []LocatedObjective{value}); err != nil {
		t.Fatal(err)
	}
	value.Path, value.Revision = "initiatives/payments.md", "commit-b"
	if err := ValidateObjectiveSet(contractScope(), []LocatedObjective{value}); err != nil {
		t.Fatal(err)
	}
	duplicate := value
	duplicate.Path = "objectives/duplicate.md"
	if err := ValidateObjectiveSet(contractScope(), []LocatedObjective{value, duplicate}); err == nil {
		t.Fatal("duplicate live ID silently won")
	}
	before := contractEdge().To
	after := before
	after.SourceBindingID = "links"
	if before.Key() != after.Key() {
		t.Fatal("verified repository move changed objective identity")
	}
}

func TestDocumentRejectsAmbiguousUnboundedOrUntrustedMetadata(t *testing.T) {
	prefix := "---\ngoobers:\n  schemaVersion: objectives/v1\n  objectiveId: " + objectiveID + "\n  title: Example\n"
	for _, raw := range []string{
		prefix + "  command: run-me\n---\nbody",
		prefix + "  title: duplicate\n---\nbody",
		"---\nfirst: &x hi\nsecond: *x\n---\nbody",
		"---\ngoobers: null\n---\nbody",
		"---\nunterminated: yes\n",
		"---\n? [complex, key]\n: value\n---\nbody",
		strings.Repeat("x", MaxSourceBytes+1),
	} {
		if _, err := ParseDocument([]byte(raw), contractScope(), "strategy"); err == nil {
			t.Fatalf("accepted invalid source %.100q", raw)
		}
	}
	edge := contractEdge()
	edge.From = edge.To
	edge.To = contractEdge().From
	edge.Kind = "references"
	value := contractObjective()
	value.Edges = []Edge{edge}
	if _, err := ProposeObjectiveMetadata(nil, contractScope(), "strategy", value); err != nil {
		t.Fatal(err)
	}
	value.Edges[0].From = contractEdge().From
	if _, err := ProposeObjectiveMetadata(nil, contractScope(), "strategy", value); err == nil {
		t.Fatal("document accepted another object's outgoing edge")
	}
}

func TestManifestScopeOwnershipAndDistinctEdgeSemantics(t *testing.T) {
	edge := contractEdge()
	manifest := Manifest{SchemaVersion: "relationships/v1", Edges: []Edge{edge}, Aliases: []Alias{{Name: "payments", Target: edge.To}}}
	raw, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseManifest(raw, contractScope())
	if err != nil || len(parsed.Edges) != 1 {
		t.Fatalf("manifest: %+v %v", parsed, err)
	}
	manifestOwner := &Owner{Kind: "manifest", SourceBindingID: "links", Path: "planning/relationships.yaml"}
	owner, err := ResolveOwner(contractScope(), edge, Ownership{NativeField: "native-contribution", ManifestOwner: manifestOwner})
	if err != nil || owner.Kind != "native" {
		t.Fatalf("native owner fell back: %+v %v", owner, err)
	}
	owner, err = ResolveOwner(contractScope(), edge, Ownership{ManifestOwner: manifestOwner})
	if err != nil || owner != *manifestOwner {
		t.Fatalf("manifest owner: %+v %v", owner, err)
	}
	if _, err := ResolveOwner(contractScope(), edge, Ownership{}); err == nil {
		t.Fatal("implicit manifest owner")
	}
	for _, kind := range []string{"milestone-member", "parent-of", "blocked-by", "implemented-by"} {
		candidate := edge
		candidate.Kind = kind
		switch kind {
		case "milestone-member":
			candidate.To.Kind = "milestone"
		case "implemented-by":
			candidate.To.Kind = "pull-request"
		default:
			candidate.To.Kind = "work-item"
		}
		if err := contractScope().ValidateEdge(candidate); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
	}
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.Edges[0].To.GaggleID = "other" },
		func(m *Manifest) { m.Edges[0].To.SourceBindingID = "not-configured" },
		func(m *Manifest) { m.Edges[0].Kind = "observed-in-run" },
		func(m *Manifest) { m.Edges = append(m.Edges, m.Edges[0]) },
		func(m *Manifest) { m.Aliases = append(m.Aliases, m.Aliases[0]) },
	} {
		var bad Manifest
		if err := yaml.Unmarshal(raw, &bad); err != nil {
			t.Fatal(err)
		}
		mutate(&bad)
		badRaw, err := yaml.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseManifest(badRaw, contractScope()); err == nil {
			t.Fatalf("accepted invalid manifest %s", badRaw)
		}
	}
}
