package workbenchgraph

import (
	"encoding/hex"
	"reflect"

	"github.com/goobers/goobers/internal/workbench"
)

type documentScan struct {
	source workbench.BoundSource
	files  map[int]workbench.DocumentFileRead
}

func (p *projection) documentPages(pages []workbench.DocumentPage) error {
	scans := map[string]*documentScan{}
	repositories := map[string]string{}
	for _, page := range pages {
		source, ok := p.sources[page.SourceBindingID]
		expected, err := workbench.SourceTargetDigest(p.scope, source)
		if !ok || err != nil || expected != page.SourceTargetDigest || !validDocumentPage(source, page) {
			return ErrInvalidSnapshot
		}
		repository := digest(struct {
			Target any
			Branch string
		}{page.Repository, page.Branch})
		if commit := repositories[repository]; commit != "" && commit != page.Commit {
			return ErrInvalidSnapshot
		}
		repositories[repository] = page.Commit
		scan := scans[page.SourceBindingID]
		if scan == nil {
			scan = &documentScan{source: source, files: map[int]workbench.DocumentFileRead{}}
			scans[page.SourceBindingID] = scan
		}
		if err := p.addDocumentPage(scan, page); err != nil {
			return err
		}
	}
	for name, scan := range scans {
		c := p.coverage[name]
		c.Status = "complete"
		for index, path := range scan.source.Spec.Paths {
			file, ok := scan.files[index]
			if !ok {
				c.Status = "partial"
				addReason(c, "paths-not-read")
				continue
			}
			if file.Status != "available" {
				c.Status = "partial"
				addReason(c, "source-omissions")
			}
			if err := p.document(scan.source, path, file, c.Commit); err != nil {
				return err
			}
		}
	}
	return nil
}
func validDocumentPage(source workbench.BoundSource, page workbench.DocumentPage) bool {
	if (source.Spec.Kind != "documents" && source.Spec.Kind != "relationships") || source.Spec.Repository == nil {
		return false
	}
	if page.Repository != *source.Spec.Repository || page.Branch != source.Repository.Branch || !validCommit(page.Commit) || !validDigest(page.SourceTargetDigest) {
		return false
	}
	if page.TotalPaths != len(source.Spec.Paths) || page.TotalPaths < 1 || page.TotalPaths > 128 || page.StartOffset < 0 || page.StartOffset >= page.TotalPaths {
		return false
	}
	return len(page.Files) <= workbench.MaxDocumentPageFiles && page.StartOffset+len(page.Files) <= page.TotalPaths
}
func validCommit(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 20 && hex.EncodeToString(decoded) == value
}
func (p *projection) addDocumentPage(scan *documentScan, page workbench.DocumentPage) error {
	c := p.coverage[page.SourceBindingID]
	if c.Commit != "" && (c.Commit != page.Commit || c.SourceTargetDigest != page.SourceTargetDigest) {
		return ErrInvalidSnapshot
	}
	c.Commit, c.SourceTargetDigest = page.Commit, page.SourceTargetDigest
	for index, file := range page.Files {
		position := page.StartOffset + index
		if file.Path != scan.source.Spec.Paths[position] {
			return ErrInvalidSnapshot
		}
		if old, ok := scan.files[position]; ok && !reflect.DeepEqual(old, file) {
			return ErrInvalidSnapshot
		}
		scan.files[position] = file
	}
	return nil
}
func (p *projection) document(source workbench.BoundSource, path string, file workbench.DocumentFileRead, commit string) error {
	switch file.Status {
	case "available", "unavailable", "invalid-source", "oversized":
	default:
		return ErrInvalidSnapshot
	}
	if file.Provenance != nil {
		if file.Provenance.Commit != commit || !validCommit(file.Provenance.BlobID) || !validDigest(file.Provenance.ContentDigest) {
			return ErrInvalidSnapshot
		}
	}
	key := source.Spec.Name + "/" + path
	value := Document{SourceBindingID: source.Spec.Name, Path: path, Status: file.Status}
	if file.Provenance != nil {
		pin := *file.Provenance
		value.Provenance = &pin
	}
	p.documents[key] = value
	if file.Status != "available" {
		if file.Ref != nil || file.Objective != nil || file.Manifest != nil || file.Body != "" {
			return ErrInvalidSnapshot
		}
		return nil
	}
	if file.Provenance == nil {
		return ErrInvalidSnapshot
	}
	if source.Spec.Kind == "relationships" {
		return p.manifestFile(source, file)
	}
	if file.Manifest != nil {
		return ErrInvalidSnapshot
	}
	if file.Objective == nil {
		if file.Ref != nil {
			return ErrInvalidSnapshot
		}
		return nil
	}
	return p.objectiveFile(source, file)
}
func (p *projection) objectiveFile(source workbench.BoundSource, file workbench.DocumentFileRead) error {
	objective := file.Objective
	expected := workbench.NodeRef{GaggleID: p.scope.GaggleID, SourceBindingID: source.Spec.Name, Kind: "objective-document", SourceID: objective.ObjectiveID}
	if file.Ref == nil || *file.Ref != expected {
		return ErrInvalidSnapshot
	}
	located := workbench.LocatedObjective{Binding: source.Spec.Name, Path: file.Path, Revision: file.Provenance.Commit, Objective: *objective}
	if workbench.ValidateObjectiveSet(p.scope, []workbench.LocatedObjective{located}) != nil {
		return ErrInvalidSnapshot
	}
	pin := *file.Provenance
	if err := p.node(Observation{ContentDigest: pin.ContentDigest, Ref: expected, Title: objective.Title, Revision: pin.Commit, Objective: true, Path: file.Path, Provenance: &pin}); err != nil {
		return err
	}
	doc := p.documents[source.Spec.Name+"/"+file.Path]
	doc.Ref = &expected
	p.documents[source.Spec.Name+"/"+file.Path] = doc
	owner := workbench.Owner{Kind: "frontmatter", SourceBindingID: source.Spec.Name, Path: file.Path}
	return p.authoredEdges(objective.Edges, owner)
}
func (p *projection) manifestFile(source workbench.BoundSource, file workbench.DocumentFileRead) error {
	if file.Objective != nil || file.Ref != nil || file.Body != "" || file.Manifest == nil || file.Manifest.SchemaVersion != "relationships/v1" {
		return ErrInvalidSnapshot
	}
	owner := workbench.Owner{Kind: "manifest", SourceBindingID: source.Spec.Name, Path: file.Path}
	if p.manifest == nil || *p.manifest != owner {
		p.coverage[source.Spec.Name].Status = "partial"
		addReason(p.coverage[source.Spec.Name], "manifest-owner-not-selected")
		return nil
	}
	if len(file.Manifest.Aliases) > 128 {
		return ErrProjectionBound
	}
	for _, alias := range file.Manifest.Aliases {
		if p.scope.ValidateRef(alias.Target) != nil {
			return ErrInvalidSnapshot
		}
		target := alias.Target
		p.graph.Aliases = append(p.graph.Aliases, Alias{Name: alias.Name, Target: Endpoint{Ref: &target}, Owner: owner})
	}
	return p.authoredEdges(file.Manifest.Edges, owner)
}
func (p *projection) authoredEdges(edges []workbench.Edge, owner workbench.Owner) error {
	if len(edges) > workbench.MaxSourceEdges {
		return ErrProjectionBound
	}
	for _, edge := range edges {
		if workbench.ValidateOwnership(p.scope, []workbench.OwnedEdge{{Edge: edge, Owner: owner}}) != nil {
			return ErrInvalidSnapshot
		}
		from, to := edge.From, edge.To
		value := Edge{EdgeID: edge.EdgeID, Kind: edge.Kind, From: Endpoint{Ref: &from}, To: Endpoint{Ref: &to}, Rationale: edge.Rationale, Origin: "authored", Owner: owner}
		if err := p.edge(value); err != nil {
			return err
		}
	}
	return nil
}
