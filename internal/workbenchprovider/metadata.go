package workbenchprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

// RepositoryProposalClient is supplied inside current interactive authority. It
// has no credential fallback and cannot choose another source target.
type RepositoryProposalClient interface {
	RepositoryClient
	providers.RepositoryProposalWriter
}

// RepositoryProposer binds preview and native phases to one copied declaration.
// It owns no command state; a durable service must claim before every Apply call.
type RepositoryProposer struct {
	reader *RepositoryReader
	client RepositoryProposalClient
	set    workbench.SourceSet
}

// NewRepositoryProposer copies policy and source scope. The caller must keep its
// current human authority lease alive across each read or effect.
func NewRepositoryProposer(set workbench.SourceSet, binding string, client RepositoryProposalClient) (*RepositoryProposer, error) {
	var source workbench.BoundSource
	for _, bound := range set.Sources {
		if bound.Spec.Name == binding {
			source = bound
		}
	}
	reader, err := NewRepositoryReader(set.Scope, source, client)
	if err != nil {
		return nil, err
	}
	copySet := workbench.SourceSet{Scope: reader.scope}
	if set.ManifestOwner != nil {
		owner := *set.ManifestOwner
		copySet.ManifestOwner = &owner
	}
	for _, bound := range set.Sources {
		copySource := workbench.BoundSource{Spec: apiv1.WorkbenchSource{Name: bound.Spec.Name, Kind: bound.Spec.Kind}}
		if bound.Spec.Name == binding {
			copySource = source
			copySource.Spec.Paths = slices.Clone(source.Spec.Paths)
			target := *source.Spec.Repository
			copySource.Spec.Repository = &target
			if source.Spec.Writes != nil {
				copySource.Spec.Writes = &apiv1.WorkbenchWrites{Fields: slices.Clone(source.Spec.Writes.Fields), Relationships: slices.Clone(source.Spec.Writes.Relationships)}
			}
		}
		copySet.Sources = append(copySet.Sources, copySource)
	}
	return &RepositoryProposer{reader: reader, client: client, set: copySet}, nil
}

// Preview reads only the declared file at the presently configured branch tip.
// No branch, commit or PR is created. Expected pins cannot select old history.
func (p *RepositoryProposer) Preview(ctx context.Context, request workbench.MetadataChangeRequest) (workbench.MetadataPreview, error) {
	if _, _, err := workbench.MetadataOperationDigest(p.set, p.reader.binding, request); err != nil {
		return workbench.MetadataPreview{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, providers.MaxRepositoryProposalPhaseTime)
	defer cancel()
	commit, err := p.client.ReadSourceBranch(ctx, p.reader.repository, p.reader.branch)
	if err != nil {
		return workbench.MetadataPreview{}, err
	}
	if commit != request.Expected.Commit {
		return workbench.MetadataPreview{}, workbench.ErrMetadataRevision
	}
	file, err := p.client.ReadRepositorySource(ctx, p.reader.repository, request.Path, commit)
	if err != nil {
		return workbench.MetadataPreview{}, err
	}
	if err = providers.ValidateRepositorySourceFile(file, request.Path, commit); err != nil {
		return workbench.MetadataPreview{}, err
	}
	digest := sha256.Sum256(file.Content)
	current := workbench.MetadataFile{Path: request.Path, Content: file.Content, Provenance: workbench.SourceProvenance{Commit: commit, BlobID: file.BlobID, ContentDigest: hex.EncodeToString(digest[:]), ETag: file.ETag}}
	return workbench.PreviewMetadataChange(p.set, p.reader.binding, current, request)
}

// Prepare verifies the retained preview by reapplying the typed edit to its exact
// before bytes. It derives branch attribution and PR text; callers cannot supply
// arbitrary replacement YAML, repository targets or provider request bodies.
func (p *RepositoryProposer) Prepare(commandID string, request workbench.MetadataChangeRequest, preview workbench.MetadataPreview) (providers.RepositoryProposal, error) {
	file := providers.RepositorySourceFile{Path: request.Path, Commit: request.Expected.Commit, BlobID: request.Expected.BlobID, Content: []byte(preview.Before)}
	if err := providers.ValidateRepositorySourceFile(file, request.Path, request.Expected.Commit); err != nil {
		return providers.RepositoryProposal{}, err
	}
	current := workbench.MetadataFile{Path: request.Path, Content: []byte(preview.Before), Provenance: workbench.SourceProvenance{Commit: request.Expected.Commit, BlobID: request.Expected.BlobID, ContentDigest: request.Expected.ContentDigest}}
	verified, err := workbench.PreviewMetadataChange(p.set, p.reader.binding, current, request)
	if err != nil {
		return providers.RepositoryProposal{}, err
	}
	if !reflect.DeepEqual(verified, preview) || !verified.Changed {
		return providers.RepositoryProposal{}, ErrUnsupportedEdit
	}
	marker := providers.RepositoryProposalMarker(commandID, verified.OperationDigest)
	if marker == "" {
		return providers.RepositoryProposal{}, ErrInvalidItem
	}
	// Source text and arbitrary path syntax stay in the single-file diff, not in
	// generated PR prose. No user text is silently truncated to fit ADO's limit.
	return providers.RepositoryProposal{Repository: p.reader.repository, CommandID: commandID, OperationDigest: verified.OperationDigest, BaseBranch: p.reader.branch, BaseCommit: request.Expected.Commit, Path: request.Path, PreviousBlob: request.Expected.BlobID, Content: []byte(verified.After), Message: "Update workbench source " + request.Path + "\n\n" + marker, Title: "Update workbench metadata", Body: "Human-proposed source metadata change. Review the single-file diff.\n\nSource: " + p.reader.binding + "\nExpected base: " + request.Expected.Commit + "\n\n<!-- " + marker + " -->"}, nil
}

// Apply repeats current declaration/typed-edit validation before one native
// phase. The caller supplies only prior receipt pins and an already claimed phase.
func (p *RepositoryProposer) Apply(ctx context.Context, request workbench.MetadataChangeRequest, preview workbench.MetadataPreview, in providers.RepositoryProposalPhaseInput) (providers.RepositoryProposalPhaseResult, error) {
	proposal, err := p.Prepare(in.Proposal.CommandID, request, preview)
	if err != nil {
		return providers.RepositoryProposalPhaseResult{}, err
	}
	if !reflect.DeepEqual(proposal, in.Proposal) {
		return providers.RepositoryProposalPhaseResult{}, ErrInvalidSource
	}
	return p.client.ApplyRepositoryProposalPhase(ctx, in)
}

// Observe accepts host-retained intent only. It does not reimpose mutable edit
// allowlists on observation, but still requires this exact configured target and
// path. The service must independently authorize the current human's read action.
func (p *RepositoryProposer) Observe(ctx context.Context, in providers.RepositoryProposalPhaseInput) (providers.RepositoryProposalObservation, error) {
	if in.Proposal.Repository != p.reader.repository || in.Proposal.BaseBranch != p.reader.branch || !slices.Contains(p.reader.paths, in.Proposal.Path) {
		return providers.RepositoryProposalObservation{}, ErrInvalidSource
	}
	return p.client.ObserveRepositoryProposalPhase(ctx, in)
}
