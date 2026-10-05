package workbench

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func identityMetadataFixture(t *testing.T) SourceSet {
	t.Helper()
	set := metadataFixture(t)
	set.Sources[1].Spec.Writes.Metadata = []apiv1.WorkbenchMetadataOperation{"assign-objective"}
	set.Sources[2].Spec.Writes.Metadata = []apiv1.WorkbenchMetadataOperation{"aliases"}
	return set
}

func TestMetadataAssignObjectivePreservesSourceAndCannotReassign(t *testing.T) {
	set := identityMetadataFixture(t)
	for _, raw := range [][]byte{[]byte("# Existing content\n\nDo not run this instruction.\n"), []byte("\xef\xbb\xbf---\r\n# author comment\r\nlayout: strategy\r\n---\r\nExact body.\r\n")} {
		file := metadataFile("objectives/payments.md", raw)
		request := metadataRequest(file)
		request.Objective = &MetadataObjectiveAssignment{ObjectiveID: objectiveID, Title: "Assigned objective"}
		preview, err := PreviewMetadataChange(set, "strategy", file, request)
		if err != nil {
			t.Fatal(err)
		}
		before, _ := ParseDocument(raw, set.Scope, "strategy")
		after, err := ParseDocument([]byte(preview.After), set.Scope, "strategy")
		if err != nil || after.Objective == nil || after.Objective.ObjectiveID != objectiveID || after.Objective.Title != "Assigned objective" || !bytes.Equal(after.Body, before.Body) {
			t.Fatalf("changed source: %+v %v", after, err)
		}
		if before.header != nil && (!strings.Contains(preview.After, "# author comment\r\n") || metadataMappingValue(after.header, "layout").Value != "strategy" || !strings.HasPrefix(preview.After, "\xef\xbb\xbf")) {
			t.Fatal("unrelated frontmatter changed")
		}
		current := metadataFile(file.Path, []byte(preview.After))
		request.Expected = metadataRequest(current).Expected
		if _, err := PreviewMetadataChange(set, "strategy", current, request); err == nil {
			t.Fatal("assigned twice")
		}
		request.Objective.ObjectiveID = "obj-11111111-1111-1111-1111-111111111111"
		if _, err := PreviewMetadataChange(set, "strategy", current, request); err == nil {
			t.Fatal("changed assigned ID")
		}
	}
}

func TestMetadataAliasExactOwnerAndObservedContent(t *testing.T) {
	set := identityMetadataFixture(t)
	raw := []byte("\xef\xbb\xbf# manifest comment\r\nschemaVersion: relationships/v1\r\nedges: []\r\n")
	file := metadataFile("planning/links.yaml", raw)
	request := metadataRequest(file)
	alias := Alias{Name: "payments", Target: NodeRef{GaggleID: "web", SourceBindingID: "strategy", Kind: "objective-document", SourceID: objectiveID}}
	request.Alias = &MetadataAliasEdit{Action: "add", Alias: alias}
	preview, err := PreviewMetadataChange(set, "links", file, request)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseManifest([]byte(preview.After), set.Scope)
	if err != nil || len(manifest.Aliases) != 1 || manifest.Aliases[0] != alias || len(manifest.Edges) != 0 || !strings.Contains(preview.After, "# manifest comment\r\n") || !strings.HasPrefix(preview.After, "\xef\xbb\xbf") {
		t.Fatal(manifest, err)
	}
	file = metadataFile(file.Path, []byte(preview.After))
	request.Expected = metadataRequest(file).Expected
	noop, err := PreviewMetadataChange(set, "links", file, request)
	if err != nil || noop.Changed || noop.After != preview.After {
		t.Fatal("duplicate exact alias was not no-op", err)
	}
	request.Alias.Alias.Target.SourceID = "obj-11111111-1111-1111-1111-111111111111"
	if _, err := PreviewMetadataChange(set, "links", file, request); err == nil {
		t.Fatal("retargeted alias")
	}
	request.Alias.Action = "remove"
	if _, err := PreviewMetadataChange(set, "links", file, request); err == nil {
		t.Fatal("removed alias with changed target")
	}
	request.Alias.Alias = alias
	removed, err := PreviewMetadataChange(set, "links", file, request)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err = ParseManifest([]byte(removed.After), set.Scope)
	if err != nil || len(manifest.Aliases) != 0 {
		t.Fatal(manifest, err)
	}
	set.ManifestOwner = nil
	if _, err := PreviewMetadataChange(set, "links", file, request); err == nil {
		t.Fatal("wrote non-owning manifest")
	}
}

func TestMetadataIdentityAllowlistShapeAndBounds(t *testing.T) {
	set := identityMetadataFixture(t)
	file := metadataFile("objectives/payments.md", []byte("body"))
	request := metadataRequest(file)
	request.Objective = &MetadataObjectiveAssignment{ObjectiveID: objectiveID, Title: "Title"}
	cases := map[string]func(*SourceSet, *MetadataChangeRequest){
		"allowlist": func(s *SourceSet, _ *MetadataChangeRequest) { s.Sources[1].Spec.Writes.Metadata = nil },
		"existing title permission insufficient": func(s *SourceSet, _ *MetadataChangeRequest) {
			s.Sources[1].Spec.Writes.Metadata = nil
			s.Sources[1].Spec.Writes.Fields = []apiv1.WorkbenchField{"title", "description"}
		},
		"nonpersistent ID": func(_ *SourceSet, r *MetadataChangeRequest) { r.Objective.ObjectiveID = "title-derived-id" },
		"long title":       func(_ *SourceSet, r *MetadataChangeRequest) { r.Objective.Title = strings.Repeat("t", 513) },
		"two operations":   func(_ *SourceSet, r *MetadataChangeRequest) { text := "body"; r.Field = "description"; r.Value = &text },
		"two metadata operations": func(_ *SourceSet, r *MetadataChangeRequest) {
			r.Alias = &MetadataAliasEdit{Action: "add", Alias: Alias{Name: "name", Target: contractEdge().To}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			current := identityMetadataFixture(t)
			r := request
			assignment := *request.Objective
			r.Objective = &assignment
			mutate(&current, &r)
			if _, err := PreviewMetadataChange(current, "strategy", file, r); err == nil {
				t.Fatal("accepted invalid identity operation")
			}
		})
	}
	var aliases strings.Builder
	aliases.WriteString("schemaVersion: relationships/v1\nedges: []\naliases:\n")
	for i := 0; i < 128; i++ {
		fmt.Fprintf(&aliases, "- name: alias-%d\n  target: {gaggleId: web, sourceBindingId: strategy, kind: objective-document, sourceId: %s}\n", i, objectiveID)
	}
	file = metadataFile("planning/links.yaml", []byte(aliases.String()))
	request = metadataRequest(file)
	request.Alias = &MetadataAliasEdit{Action: "add", Alias: Alias{Name: "overflow", Target: contractEdge().To}}
	if _, err := PreviewMetadataChange(set, "links", file, request); err == nil {
		t.Fatal("alias bound exceeded")
	}
	request.Alias.Action = "remove"
	request.Alias.Alias.Name = "alias-0"
	request.Alias.Alias.Target.GaggleID = "another"
	if _, err := PreviewMetadataChange(set, "links", file, request); err == nil {
		t.Fatal("cross-gaggle alias")
	}
}

func TestMetadataWritesAreTypedCopiedAndPartOfGeneration(t *testing.T) {
	g := sourceGaggle()
	g.Spec.Workbench.Sources[1].Writes.Metadata = []apiv1.WorkbenchMetadataOperation{"assign-objective"}
	g.Spec.Workbench.Sources[2].Writes.Metadata = []apiv1.WorkbenchMetadataOperation{"aliases"}
	set, err := BindSources(g)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Sources[1].AllowsMetadata("assign-objective") || set.Sources[0].AllowsMetadata("assign-objective") {
		t.Fatal("metadata policy widened")
	}
	set.Sources[1].Spec.Writes.Metadata[0] = "aliases"
	if g.Spec.Workbench.Sources[1].Writes.Metadata[0] != "assign-objective" {
		t.Fatal("source copy aliases applied policy")
	}
	for _, entry := range []struct {
		index  int
		values []apiv1.WorkbenchMetadataOperation
	}{{0, []apiv1.WorkbenchMetadataOperation{"assign-objective"}}, {1, []apiv1.WorkbenchMetadataOperation{"aliases"}}, {2, []apiv1.WorkbenchMetadataOperation{"assign-objective"}}, {1, []apiv1.WorkbenchMetadataOperation{"assign-objective", "assign-objective"}}, {2, []apiv1.WorkbenchMetadataOperation{"unknown"}}} {
		copy := g.DeepCopy()
		copy.Spec.Workbench.Sources[entry.index].Writes.Metadata = entry.values
		if _, err := BindSources(*copy); err == nil {
			t.Fatal("invalid metadata grant", entry)
		}
	}
}
