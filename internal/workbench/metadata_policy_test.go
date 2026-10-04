package workbench

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestMetadataPreviewRefusesRevisionPolicyAndScopeChanges(t *testing.T) {
	cases := map[string]func(*SourceSet, *MetadataFile, *MetadataChangeRequest){
		"commit": func(_ *SourceSet, _ *MetadataFile, r *MetadataChangeRequest) {
			r.Expected.Commit = strings.Repeat("c", 40)
		},
		"blob": func(_ *SourceSet, _ *MetadataFile, r *MetadataChangeRequest) {
			r.Expected.BlobID = strings.Repeat("c", 40)
		},
		"digest": func(_ *SourceSet, _ *MetadataFile, r *MetadataChangeRequest) {
			r.Expected.ContentDigest = strings.Repeat("c", 64)
		},
		"short commit": func(_ *SourceSet, f *MetadataFile, r *MetadataChangeRequest) {
			f.Provenance.Commit, r.Expected.Commit = "abc", "abc"
		},
		"tampered source": func(_ *SourceSet, f *MetadataFile, _ *MetadataChangeRequest) { f.Content = append(f.Content, 'x') },
		"undeclared path": func(_ *SourceSet, f *MetadataFile, r *MetadataChangeRequest) { f.Path, r.Path = "other.md", "other.md" },
		"path traversal": func(_ *SourceSet, f *MetadataFile, r *MetadataChangeRequest) {
			f.Path, r.Path = "../file.md", "../file.md"
		},
		"wrong current path": func(_ *SourceSet, f *MetadataFile, _ *MetadataChangeRequest) { f.Path = "other.md" },
		"write removed":      func(s *SourceSet, _ *MetadataFile, _ *MetadataChangeRequest) { s.Sources[1].Spec.Writes = nil },
		"target changed":     func(s *SourceSet, _ *MetadataFile, _ *MetadataChangeRequest) { s.Sources[1].Repository.Name = "other" },
		"custom endpoint": func(s *SourceSet, _ *MetadataFile, _ *MetadataChangeRequest) {
			s.Sources[1].Repository.BaseURL = "https://example.invalid"
		},
		"missing branch":    func(s *SourceSet, _ *MetadataFile, _ *MetadataChangeRequest) { s.Sources[1].Repository.Branch = "" },
		"foreign gaggle":    func(s *SourceSet, _ *MetadataFile, _ *MetadataChangeRequest) { s.Scope.GaggleID = "" },
		"unsupported field": func(_ *SourceSet, _ *MetadataFile, r *MetadataChangeRequest) { r.Field = "state" },
		"missing value":     func(_ *SourceSet, _ *MetadataFile, r *MetadataChangeRequest) { r.Value = nil },
		"two operations": func(_ *SourceSet, _ *MetadataFile, r *MetadataChangeRequest) {
			r.Relationship = &MetadataRelationshipEdit{Action: "add", Edge: contractEdge()}
		},
		"title blank":     func(_ *SourceSet, _ *MetadataFile, r *MetadataChangeRequest) { *r.Value = "" },
		"title controls":  func(_ *SourceSet, _ *MetadataFile, r *MetadataChangeRequest) { *r.Value = "bad\ntitle" },
		"title oversized": func(_ *SourceSet, _ *MetadataFile, r *MetadataChangeRequest) { *r.Value = strings.Repeat("x", 513) },
		"body oversized": func(_ *SourceSet, _ *MetadataFile, r *MetadataChangeRequest) {
			r.Field = "description"
			*r.Value = strings.Repeat("x", MaxSourceBytes+1)
		},
		"body invalid UTF8": func(_ *SourceSet, _ *MetadataFile, r *MetadataChangeRequest) {
			r.Field = "description"
			*r.Value = string([]byte{255})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			set := metadataFixture(t)
			file := metadataFile("objectives/payments.md", metadataObjective(t))
			request := metadataRequest(file)
			title := "Changed title"
			request.Field, request.Value = "title", &title
			mutate(&set, &file, &request)
			if _, err := PreviewMetadataChange(set, "strategy", file, request); err == nil {
				t.Fatal("invalid mutation accepted")
			}
		})
	}
}

func TestMetadataManifestCannotOverrideNativeOrDocumentOwner(t *testing.T) {
	for _, kind := range []string{"parent-of", "blocked-by", "milestone-member", "implemented-by"} {
		t.Run(kind, func(t *testing.T) {
			set := metadataFixture(t)
			file := metadataFile("planning/links.yaml", []byte("schemaVersion: relationships/v1\nedges: []\n"))
			request := metadataRequest(file)
			edge := contractEdge()
			edge.Kind, edge.To.Kind = kind, "work-item"
			if kind == "milestone-member" {
				edge.To.Kind = "milestone"
			}
			if kind == "implemented-by" {
				edge.To.Kind = "pull-request"
			}
			request.Relationship = &MetadataRelationshipEdit{Action: "add", Edge: edge}
			if _, err := PreviewMetadataChange(set, "links", file, request); !errors.Is(err, ErrMetadataEdit) {
				t.Fatal("native relationship fell back to manifest", err)
			}
		})
	}
	for _, owner := range []string{"missing", "other manifest", "objective", "disguised document", "cross gaggle", "not allowed"} {
		t.Run(owner, func(t *testing.T) {
			set := metadataFixture(t)
			file := metadataFile("planning/links.yaml", []byte("schemaVersion: relationships/v1\nedges: []\n"))
			request := metadataRequest(file)
			request.Relationship = &MetadataRelationshipEdit{Action: "add", Edge: contractEdge()}
			switch owner {
			case "missing":
				set.ManifestOwner = nil
			case "other manifest":
				set.ManifestOwner.Path = "other.yaml"
			case "objective":
				request.Relationship.Edge.From, request.Relationship.Edge.To = request.Relationship.Edge.To, request.Relationship.Edge.From
			case "disguised document":
				request.Relationship.Edge.From.SourceBindingID = "strategy"
			case "cross gaggle":
				request.Relationship.Edge.To.GaggleID = "other"
			case "not allowed":
				set.Sources[2].Spec.Writes.Relationships = nil
			}
			if _, err := PreviewMetadataChange(set, "links", file, request); err == nil {
				t.Fatal("wrong source owner accepted")
			}
		})
	}
}

func TestMetadataEdgeConflictsAndAggregateSourceBound(t *testing.T) {
	set := metadataFixture(t)
	existing := contractEdge()
	raw, _ := yaml.Marshal(Manifest{SchemaVersion: "relationships/v1", Edges: []Edge{existing}})
	file := metadataFile("planning/links.yaml", raw)
	for _, change := range []string{"rationale", "same tuple", "remove missing", "unknown action"} {
		request := metadataRequest(file)
		request.Relationship = &MetadataRelationshipEdit{Action: "add", Edge: existing}
		switch change {
		case "rationale":
			request.Relationship.Edge.Rationale = "changed identity payload"
		case "same tuple":
			request.Relationship.Edge.EdgeID = "edge-11111111-1111-1111-1111-111111111111"
		case "remove missing":
			request.Relationship.Action = "remove"
			request.Relationship.Edge.EdgeID = "edge-11111111-1111-1111-1111-111111111111"
		case "unknown action":
			request.Relationship.Action = "replace"
		}
		if _, err := PreviewMetadataChange(set, "links", file, request); err == nil {
			t.Fatal("edge conflict accepted", change)
		}
	}
	file = metadataFile("objectives/payments.md", metadataObjective(t))
	request := metadataRequest(file)
	body := strings.Repeat("x", MaxSourceBytes)
	request.Field, request.Value = "description", &body
	if _, err := PreviewMetadataChange(set, "strategy", file, request); err == nil {
		t.Fatal("combined body and preserved header exceeded source bound")
	}
	file = metadataFile(file.Path, bytes.Repeat([]byte("x"), MaxSourceBytes+1))
	request.Expected = metadataRequest(file).Expected
	if _, err := PreviewMetadataChange(set, "strategy", file, request); err == nil {
		t.Fatal("oversized input accepted")
	}
}

func TestMetadataDigestsBindExactOperationAndPhysicalTargetOnly(t *testing.T) {
	set := metadataFixture(t)
	file := metadataFile("objectives/payments.md", metadataObjective(t))
	request := metadataRequest(file)
	title := "Changed title"
	request.Field, request.Value = "title", &title
	target, operation, err := MetadataOperationDigest(set, "strategy", request)
	if err != nil {
		t.Fatal(err)
	}
	set.Sources[1].Spec.Paths = append(set.Sources[1].Spec.Paths, "another.md")
	set.Sources[0].Spec.Objectives = &apiv1.WorkbenchObjectiveSelector{Labels: []string{"new"}}
	target2, operation2, err := MetadataOperationDigest(set, "strategy", request)
	if err != nil || target != target2 || operation != operation2 {
		t.Fatal("unrelated config changed command identity", err)
	}
	title = "Different edit"
	target2, operation2, err = MetadataOperationDigest(set, "strategy", request)
	if err != nil || target != target2 || operation == operation2 {
		t.Fatal("different edit did not change operation identity", err)
	}
	set.Sources[1].Repository.Branch = "other"
	target2, _, err = MetadataOperationDigest(set, "strategy", request)
	if err != nil || target == target2 {
		t.Fatal("branch changed without target change", err)
	}
}
