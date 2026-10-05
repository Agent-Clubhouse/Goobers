package workbenchservice

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
	"github.com/goobers/goobers/providers"
)

type verifiedSuggestion struct {
	bound   proposalBinding
	load    interactiveaccess.RepositoryCredentialLoader
	request workbench.MetadataChangeRequest
}

type suggestionVerifier struct {
	service  *Service
	snapshot suggestionSnapshot
	commits  map[suggestionRepositoryKey]string
	files    map[string]workbench.MetadataFile
}

func (s *SuggestionService) verify(ctx context.Context, snap suggestionSnapshot, bound workbench.BoundSuggestion) (verifiedSuggestion, error) {
	if reason := suggestionUnsupported(bound); reason != "" {
		return verifiedSuggestion{}, readError(http.StatusConflict, "workbench_suggestion_unsupported", "This suggestion requires a source operation not supported by the current editor.")
	}
	if err := snap.authorizeSuggestion(bound); err != nil {
		return verifiedSuggestion{}, err
	}
	edge, _, err := workbench.MaterializeSuggestion(snap.set, bound, nil)
	if err != nil {
		return verifiedSuggestion{}, err
	}
	v := suggestionVerifier{service: s.Proposals.ReadService, snapshot: snap, commits: map[suggestionRepositoryKey]string{}, files: map[string]workbench.MetadataFile{}}
	for _, endpoint := range []workbench.SuggestionEndpoint{bound.Proposal.From, bound.Proposal.To} {
		if err = v.endpoint(ctx, endpoint); err != nil {
			return verifiedSuggestion{}, err
		}
	}
	policy := workbench.Ownership{ManifestOwner: snap.set.ManifestOwner}
	if edge.From.Kind == "objective-document" {
		policy.DocumentPath = bound.Proposal.From.Evidence.Path
	}
	owner, err := workbench.ResolveOwner(snap.set.Scope, edge, policy)
	if err != nil {
		return verifiedSuggestion{}, workbench.ErrMetadataEdge
	}
	selected, err := snap.source(owner.SourceBindingID)
	if err != nil {
		return verifiedSuggestion{}, err
	}
	if !selected.Source.AllowsRelationship(apiv1.WorkbenchRelationship(edge.Kind)) {
		return verifiedSuggestion{}, workbench.ErrMetadataEdit
	}
	file, err := v.repositoryFile(ctx, selected, owner.Path)
	if err != nil {
		return verifiedSuggestion{}, err
	}
	load, err := snap.access("source.proposeChange", interactiveaccess.Target{Kind: "repository", Repository: *selected.Source.Spec.Repository})
	if err != nil {
		return verifiedSuggestion{}, err
	}
	request := workbench.MetadataChangeRequest{Path: owner.Path, Expected: workbench.MetadataRevision{Commit: file.Provenance.Commit, BlobID: file.Provenance.BlobID, ContentDigest: file.Provenance.ContentDigest}, Relationship: &workbench.MetadataRelationshipEdit{Action: "add", Edge: edge}}
	// This also checks the exact document identity and configured manifest owner.
	if _, err = workbench.PreviewMetadataChange(snap.set, owner.SourceBindingID, file, request); err != nil {
		return verifiedSuggestion{}, err
	}
	return verifiedSuggestion{bound: proposalBinding{set: snap.set, read: selected}, load: load, request: request}, nil
}

func (v *suggestionVerifier) endpoint(ctx context.Context, endpoint workbench.SuggestionEndpoint) error {
	selected, load, err := v.snapshot.readAccess(endpoint.Ref.SourceBindingID)
	if err != nil {
		return err
	}
	if endpoint.Ref.Kind == "work-item" {
		return v.native(ctx, selected, load, endpoint)
	}
	file, err := v.repositoryFile(ctx, selected, endpoint.Evidence.Path)
	if err != nil {
		return err
	}
	expected := endpoint.Evidence.RepositoryRevision
	if expected == nil || file.Provenance.Commit != expected.Commit || file.Provenance.BlobID != expected.BlobID || file.Provenance.ContentDigest != expected.ContentDigest {
		return workbench.ErrMetadataRevision
	}
	doc, err := workbench.ParseDocument(file.Content, selected.Scope, selected.Source.Spec.Name)
	if err != nil || doc.Objective == nil || doc.Objective.ObjectiveID != endpoint.Ref.SourceID {
		return workbenchprovider.ErrIdentityChanged
	}
	return nil
}
func (v *suggestionVerifier) native(ctx context.Context, selected ReadBinding, load interactiveaccess.RepositoryCredentialLoader, endpoint workbench.SuggestionEndpoint) error {
	locator := endpoint.Evidence.NativeLocator
	if locator == "" && selected.Source.BacklogIdentity.Provider == "ado" {
		locator = endpoint.Ref.SourceID
	}
	if locator == "" {
		return readError(http.StatusConflict, "workbench_suggestion_locator_required", "This suggestion needs a native lookup locator before its stable source identity can be verified.")
	}
	credential, err := load(ctx)
	if err != nil {
		return err
	}
	if v.service.Backlog == nil {
		return workbenchprovider.ErrInvalidSource
	}
	return v.service.useBacklog(ctx, selected, credential, func(ctx context.Context, r *workbenchprovider.BacklogReader) error {
		item, err := r.Get(ctx, workbench.BacklogItemRequest{ID: locator, ExpectedSourceID: endpoint.Ref.SourceID})
		if err != nil {
			return err
		}
		if item.Ref != *endpoint.Ref {
			return workbenchprovider.ErrIdentityChanged
		}
		if item.Revision != endpoint.Evidence.NativeRevision {
			return workbench.ErrMetadataRevision
		}
		return nil
	})
}
func (v *suggestionVerifier) repositoryFile(ctx context.Context, selected ReadBinding, path string) (workbench.MetadataFile, error) {
	if selected.Source.Spec.Repository == nil || !slices.Contains(selected.Source.Spec.Paths, path) {
		return workbench.MetadataFile{}, workbench.ErrSuggestion
	}
	key := selected.Source.Spec.Name + ":" + path
	if file, ok := v.files[key]; ok {
		return file, nil
	}
	_, load, err := v.snapshot.readAccess(selected.Source.Spec.Name)
	if err != nil {
		return workbench.MetadataFile{}, err
	}
	credential, err := load(ctx)
	if err != nil {
		return workbench.MetadataFile{}, err
	}
	if v.service.Repository == nil {
		return workbench.MetadataFile{}, workbenchprovider.ErrInvalidSource
	}
	client, err := v.service.Repository(ctx, selected, credential)
	if err != nil {
		return workbench.MetadataFile{}, err
	}
	target := selected.Source.Spec.Repository
	repo := providers.RepositoryRef{Provider: providers.ProviderKind(target.Provider), Owner: target.Owner, Project: target.Project, Name: target.Name}
	if client.Kind() != repo.Provider {
		return workbench.MetadataFile{}, workbenchprovider.ErrInvalidSource
	}
	commit, err := client.ReadSourceBranch(ctx, repo, selected.Source.Repository.Branch)
	if err != nil {
		return workbench.MetadataFile{}, err
	}
	if !providers.ValidSourceCommit(commit) {
		return workbench.MetadataFile{}, workbench.ErrMetadataRevision
	}
	if prior := v.commits[suggestionRepositoryKey{repo, selected.Source.Repository.Branch}]; prior != "" && prior != commit {
		return workbench.MetadataFile{}, workbench.ErrMetadataRevision
	}
	v.commits[suggestionRepositoryKey{repo, selected.Source.Repository.Branch}] = commit
	raw, err := client.ReadRepositorySource(ctx, repo, path, commit)
	if err != nil {
		return workbench.MetadataFile{}, err
	}
	if err = providers.ValidateRepositorySourceFile(raw, path, commit); err != nil {
		return workbench.MetadataFile{}, err
	}
	file := workbench.MetadataFile{Path: path, Content: raw.Content, Provenance: workbench.SourceProvenance{Commit: commit, BlobID: raw.BlobID, ContentDigest: fmt.Sprintf("%x", sha256.Sum256(raw.Content))}}
	v.files[key] = file
	return file, nil
}

type suggestionRepositoryKey struct {
	Repository providers.RepositoryRef
	Branch     string
}
