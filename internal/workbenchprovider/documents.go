package workbenchprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

// ErrSourceChanged requires restarting a scan after the configured branch moves.
// A continuation never grants access to arbitrary historical source commits.
var ErrSourceChanged = errors.New("workbenchprovider: configured branch changed during source scan")

// RepositoryClient is an already authorized, scoped native repository reader.
type RepositoryClient interface {
	Kind() providers.ProviderKind
	providers.RepositorySourceReader
}

// RepositoryReader reads exactly a copied declaration of document/manifest files.
// It has no credential fallback, crawler, cache or retained planning state.
type RepositoryReader struct {
	client                              RepositoryClient
	scope                               workbench.Scope
	binding, kind, branch, targetDigest string
	target                              apiv1.InteractiveRepositoryIdentity
	repository                          providers.RepositoryRef
	paths                               []string
}

// NewRepositoryReader validates a bound configured target and copies source scope.
func NewRepositoryReader(scope workbench.Scope, source workbench.BoundSource, client RepositoryClient) (*RepositoryReader, error) {
	if scope.Validate() != nil || !scope.Bindings[source.Spec.Name] || client == nil || !validRepositorySource(source) {
		return nil, ErrInvalidSource
	}
	target := *source.Spec.Repository
	if string(client.Kind()) != string(target.Provider) {
		return nil, ErrInvalidSource
	}
	r := &RepositoryReader{client: client, scope: workbench.Scope{GaggleID: scope.GaggleID, Bindings: map[string]bool{}}, binding: source.Spec.Name, kind: source.Spec.Kind, branch: source.Repository.Branch, target: target, paths: append([]string(nil), source.Spec.Paths...), repository: providers.RepositoryRef{Provider: client.Kind(), Owner: target.Owner, Project: target.Project, Name: target.Name}}
	for key, value := range scope.Bindings {
		r.scope.Bindings[key] = value
	}
	raw, _ := json.Marshal(struct {
		Gaggle, Binding, Kind, Branch string
		Target                        apiv1.InteractiveRepositoryIdentity
		Paths                         []string
	}{scope.GaggleID, r.binding, r.kind, r.branch, target, r.paths})
	digest := sha256.Sum256(raw)
	r.targetDigest = hex.EncodeToString(digest[:])
	return r, nil
}

func validRepositorySource(source workbench.BoundSource) bool {
	if source.Spec.Repository == nil || source.Repository.BaseURL != "" || source.Repository.Branch == "" || len(source.Repository.Branch) > 1024 {
		return false
	}
	target := source.Spec.Repository
	if !component(target.Owner) || !component(target.Name) || source.Repository.Provider != target.Provider || source.Repository.Owner != target.Owner || source.Repository.Project != target.Project || source.Repository.Name != target.Name {
		return false
	}
	if target.Provider != apiv1.ProviderGitHub && target.Provider != apiv1.ProviderADO {
		return false
	}
	if (target.Provider == apiv1.ProviderGitHub && target.Project != "") || (target.Provider == apiv1.ProviderADO && !component(target.Project)) {
		return false
	}
	return validRepositoryPaths(source.Spec.Kind, source.Spec.Paths)
}

func validRepositoryPaths(kind string, paths []string) bool {
	if len(paths) == 0 || len(paths) > 128 || (kind == "relationships" && len(paths) != 1) {
		return false
	}
	if kind != "documents" && kind != "relationships" {
		return false
	}
	seen := map[string]bool{}
	for _, name := range paths {
		if !providers.ValidRepositorySourcePath(name) || seen[name] || !sourceExtension(kind, name) {
			return false
		}
		seen[name] = true
	}
	return true
}

func sourceExtension(kind, name string) bool {
	name = strings.ToLower(name)
	if kind == "documents" {
		return strings.HasSuffix(name, ".md")
	}
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// Page re-observes the configured branch and reads a bounded set of declared
// paths at that exact commit. Parse failures are source data, never instructions.
func (r *RepositoryReader) Page(ctx context.Context, request workbench.DocumentPageRequest) (workbench.DocumentPage, error) {
	cursor, err := r.documentCursor(request)
	if err != nil {
		return workbench.DocumentPage{}, err
	}
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	commit, err := r.client.ReadSourceBranch(ctx, r.repository, r.branch)
	if err != nil {
		return workbench.DocumentPage{}, err
	}
	if !providers.ValidSourceCommit(commit) {
		return workbench.DocumentPage{}, ErrInvalidItem
	}
	if cursor.Commit != "" && cursor.Commit != commit {
		return workbench.DocumentPage{}, ErrSourceChanged
	}
	page := workbench.DocumentPage{SourceBindingID: r.binding, Repository: r.target, Branch: r.branch, Commit: commit, SourceTargetDigest: r.targetDigest, Files: []workbench.DocumentFileRead{}, StartOffset: cursor.Offset, TotalPaths: len(r.paths), Coverage: "partial"}
	next, omitted := r.readDocumentWindow(ctx, &page, cursor.Offset, cursor.Limit)
	if err := ctx.Err(); err != nil {
		return workbench.DocumentPage{}, err
	}
	page.Exhausted = next == len(r.paths)
	if !page.Exhausted {
		page.NextCursor = r.encodeDocumentCursor(documentCursor{Version: 1, Target: r.targetDigest, Commit: commit, Offset: next, Limit: cursor.Limit})
		page.Reasons = append(page.Reasons, "more-paths")
	}
	if omitted {
		page.Reasons = append(page.Reasons, "source-omissions")
	}
	if cursor.Offset == 0 && page.Exhausted && !omitted {
		page.Coverage = "complete"
	} else if cursor.Offset > 0 {
		page.Reasons = append(page.Reasons, "window-only")
	}
	return page, nil
}

func (r *RepositoryReader) readDocumentWindow(ctx context.Context, page *workbench.DocumentPage, start, limit int) (int, bool) {
	used, omitted := 0, false
	end := min(start+limit, len(r.paths))
	for index := start; index < end; index++ {
		if ctx.Err() != nil {
			page.Reasons = append(page.Reasons, "read-cancelled")
			return index, true
		}
		file := r.readDocument(ctx, r.paths[index], page.Commit)
		raw, _ := json.Marshal(file)
		if len(raw) > workbench.MaxDocumentPageBytes-4096 {
			file = workbench.DocumentFileRead{Path: file.Path, Status: "oversized"}
			raw, _ = json.Marshal(file)
		}
		if used+len(raw) > workbench.MaxDocumentPageBytes-4096 {
			page.Reasons = append(page.Reasons, "page-byte-limit")
			return index, omitted
		}
		page.Files = append(page.Files, file)
		used += len(raw)
		omitted = omitted || file.Status != "available"
	}
	return end, omitted
}

func (r *RepositoryReader) readDocument(ctx context.Context, name, commit string) workbench.DocumentFileRead {
	result := workbench.DocumentFileRead{Path: name, Status: "unavailable"}
	file, err := r.client.ReadRepositorySource(ctx, r.repository, name, commit)
	if err != nil {
		return result
	}
	if providers.ValidateRepositorySourceFile(file, name, commit) != nil {
		return result
	}
	if len(file.Content) > workbench.MaxSourceBytes {
		result.Status = "oversized"
		return result
	}
	digest := sha256.Sum256(file.Content)
	result.Provenance = &workbench.SourceProvenance{Commit: commit, BlobID: file.BlobID, ContentDigest: hex.EncodeToString(digest[:]), ETag: file.ETag}
	result.Status = "invalid-source"
	if r.kind == "relationships" {
		manifest, err := workbench.ParseManifest(file.Content, r.scope)
		if err != nil {
			return result
		}
		result.Manifest = &manifest
	} else {
		document, err := workbench.ParseDocument(file.Content, r.scope, r.binding)
		if err != nil {
			return result
		}
		result.Body = string(document.Body)
		result.Objective = document.Objective
		if document.Objective != nil {
			result.Ref = &workbench.NodeRef{GaggleID: r.scope.GaggleID, SourceBindingID: r.binding, Kind: "objective-document", SourceID: document.Objective.ObjectiveID}
		}
	}
	result.Status = "available"
	return result
}
