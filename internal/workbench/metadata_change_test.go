package workbench

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func metadataFixture(t *testing.T) SourceSet {
	t.Helper()
	g := sourceGaggle()
	g.Name = "web"
	g.Spec.Workbench.Sources[1].Writes.Relationships = []apiv1.WorkbenchRelationship{"references", "contributes-to"}
	g.Spec.Workbench.Sources[2].Writes.Relationships = []apiv1.WorkbenchRelationship{"references", "contributes-to", "parent-of", "blocked-by", "milestone-member", "implemented-by"}
	set, err := BindSources(g)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func metadataFile(path string, raw []byte) MetadataFile {
	return MetadataFile{Path: path, Content: raw, Provenance: SourceProvenance{
		Commit: strings.Repeat("a", 40), BlobID: strings.Repeat("b", 40), ContentDigest: metadataContentDigest(raw), ETag: "transport-only",
	}}
}

func metadataRequest(file MetadataFile) MetadataChangeRequest {
	return MetadataChangeRequest{Path: file.Path, Expected: MetadataRevision{
		Commit: file.Provenance.Commit, BlobID: file.Provenance.BlobID, ContentDigest: file.Provenance.ContentDigest,
	}}
}

func metadataObjective(t *testing.T) []byte {
	t.Helper()
	raw, err := ProposeObjectiveMetadata([]byte("\xef\xbb\xbf---\r\n# unrelated comment\r\nlayout: page\r\nauthors: [alice, bob]\r\n---\r\n# Original\r\n\r\nExact $body.\r\n"), contractScope(), "strategy", contractObjective())
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Replace(raw, []byte("title: Reliable"), []byte("# objective title comment\r\n    title: Reliable"), 1)
}

func TestMetadataPreviewTitlePreservesIdentityUnrelatedFieldsCommentsAndBody(t *testing.T) {
	set := metadataFixture(t)
	file := metadataFile("objectives/payments.md", metadataObjective(t))
	want := "A revised objective"
	request := metadataRequest(file)
	request.Field, request.Value = "title", &want
	preview, err := PreviewMetadataChange(set, "strategy", file, request)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ParseDocument([]byte(preview.After), set.Scope, "strategy")
	if err != nil || doc.Objective.Title != want || doc.Objective.ObjectiveID != objectiveID {
		t.Fatalf("objective: %+v %v", doc, err)
	}
	before, _ := ParseDocument(file.Content, set.Scope, "strategy")
	if !bytes.Equal(doc.Body, before.Body) || !strings.Contains(preview.After, "# unrelated comment\r\n") || !strings.Contains(preview.After, "# objective title comment\r\n") || !strings.HasPrefix(preview.After, "\xef\xbb\xbf") {
		t.Fatal("body, comments, line endings or BOM changed")
	}
	if metadataMappingValue(doc.header, "layout").Value != "page" || len(metadataMappingValue(doc.header, "authors").Content) != 2 {
		t.Fatal("unrelated metadata changed")
	}
	if !preview.Changed || preview.Before != string(file.Content) || preview.ProposedContentDigest != metadataContentDigest([]byte(preview.After)) {
		t.Fatal(preview)
	}
	target, operation, err := MetadataOperationDigest(set, "strategy", request)
	if err != nil || target != preview.TargetDigest || operation != preview.OperationDigest {
		t.Fatal("pure command identity differs from preview", err)
	}
}

func TestMetadataBodyPreservesExactHeaderAndCannotCreateFrontmatter(t *testing.T) {
	set := metadataFixture(t)
	for _, raw := range [][]byte{metadataObjective(t), []byte("plain body"), []byte("---\nlayout: page\n---"), []byte("\xef\xbb\xbfplain body")} {
		file := metadataFile("objectives/payments.md", raw)
		request := metadataRequest(file)
		body := "# New body\n\nWhitespace and [untrusted](https://example.invalid) text.\n"
		request.Field, request.Value = "description", &body
		preview, err := PreviewMetadataChange(set, "strategy", file, request)
		if err != nil {
			t.Fatal(err)
		}
		before, _ := ParseDocument(raw, set.Scope, "strategy")
		after, _ := ParseDocument([]byte(preview.After), set.Scope, "strategy")
		prefix := raw[:len(raw)-len(before.Body)]
		if !strings.HasPrefix(preview.After, string(prefix)) || string(after.Body) != body || !reflect.DeepEqual(before.Objective, after.Objective) {
			t.Fatal("body edit changed frontmatter")
		}
	}
	file := metadataFile("objectives/payments.md", []byte("plain body"))
	request := metadataRequest(file)
	for _, body := range []string{"---\nlayout: injected\n---\nbody", "\xef\xbb\xbfbody"} {
		request.Field, request.Value = "description", &body
		if _, err := PreviewMetadataChange(set, "strategy", file, request); err == nil {
			t.Fatal("description permission created metadata structure")
		}
	}
	request.Field = "title"
	title := "No inferred objective"
	request.Value = &title
	if _, err := PreviewMetadataChange(set, "strategy", file, request); !errors.Is(err, ErrMetadataEdit) {
		t.Fatal("plain document gained an objective", err)
	}
}

func TestMetadataObjectiveEdgeHasExactFrontmatterOwnerAndPreservesOtherEdges(t *testing.T) {
	set := metadataFixture(t)
	file := metadataFile("objectives/payments.md", metadataObjective(t))
	edge := contractEdge()
	edge.Kind, edge.From, edge.To = "references", edge.To, edge.From
	request := metadataRequest(file)
	request.Relationship = &MetadataRelationshipEdit{Action: "add", Edge: edge}
	preview, err := PreviewMetadataChange(set, "strategy", file, request)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := ParseDocument([]byte(preview.After), set.Scope, "strategy")
	if len(doc.Objective.Edges) != 1 || doc.Objective.Edges[0] != edge {
		t.Fatal(doc.Objective)
	}
	file = metadataFile(file.Path, []byte(preview.After))
	request.Expected = metadataRequest(file).Expected
	duplicate, err := PreviewMetadataChange(set, "strategy", file, request)
	if err != nil || duplicate.Changed || duplicate.After != string(file.Content) {
		t.Fatal("exact add was not a no-op", err)
	}
	request.Relationship.Action = "remove"
	removed, err := PreviewMetadataChange(set, "strategy", file, request)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ = ParseDocument([]byte(removed.After), set.Scope, "strategy")
	if len(doc.Objective.Edges) != 0 || doc.Objective.ObjectiveID != objectiveID {
		t.Fatal(doc.Objective)
	}
	request.Relationship.Edge.From.SourceID = "obj-11111111-1111-1111-1111-111111111111"
	if _, err := PreviewMetadataChange(set, "strategy", file, request); !errors.Is(err, ErrMetadataEdge) {
		t.Fatal("another objective's edge edited", err)
	}
}

func TestMetadataManifestPreservesAliasesCommentsAndUnaffectedEdge(t *testing.T) {
	set := metadataFixture(t)
	existing := contractEdge()
	manifest := Manifest{SchemaVersion: "relationships/v1", Aliases: []Alias{{Name: "payments", Target: existing.To}}, Edges: []Edge{existing}}
	raw, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte("aliases:"), []byte("# alias comment\naliases:"), 1)
	raw = bytes.Replace(raw, []byte("- edgeId:"), []byte("# retained edge comment\n    - edgeId:"), 1)
	raw = bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))
	file := metadataFile("planning/links.yaml", raw)
	edge := existing
	edge.EdgeID, edge.Kind = "edge-11111111-1111-1111-1111-111111111111", "references"
	request := metadataRequest(file)
	request.Relationship = &MetadataRelationshipEdit{Action: "add", Edge: edge}
	preview, err := PreviewMetadataChange(set, "links", file, request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseManifest([]byte(preview.After), set.Scope)
	if err != nil || !reflect.DeepEqual(parsed.Aliases, manifest.Aliases) || len(parsed.Edges) != 2 || parsed.Edges[0] != existing || parsed.Edges[1] != edge {
		t.Fatal(parsed, err)
	}
	if !strings.Contains(preview.After, "# alias comment\r\n") || !strings.Contains(preview.After, "# retained edge comment\r\n") {
		t.Fatal("unrelated source comments lost")
	}
	file = metadataFile(file.Path, []byte(preview.After))
	request.Expected, request.Relationship.Action = metadataRequest(file).Expected, "remove"
	removed, err := PreviewMetadataChange(set, "links", file, request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ = ParseManifest([]byte(removed.After), set.Scope)
	if !reflect.DeepEqual(parsed, manifest) {
		t.Fatal("remove altered unrelated metadata", parsed)
	}
}
