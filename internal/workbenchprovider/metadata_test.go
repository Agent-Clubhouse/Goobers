package workbenchprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/providers"
)

type metadataClient struct {
	sourceClient
	mutations, observations int
}

func (m *metadataClient) ApplyRepositoryProposalPhase(context.Context, providers.RepositoryProposalPhaseInput) (providers.RepositoryProposalPhaseResult, error) {
	m.mutations++
	return providers.RepositoryProposalPhaseResult{MutationAttempted: true, Acknowledged: true, TreeID: strings.Repeat("c", 40)}, nil
}
func (m *metadataClient) ObserveRepositoryProposalPhase(context.Context, providers.RepositoryProposalPhaseInput) (providers.RepositoryProposalObservation, error) {
	m.observations++
	return providers.RepositoryProposalObservation{Found: true, Matches: true}, nil
}

func metadataAdapterFixture(t *testing.T) (*RepositoryProposer, *metadataClient, workbench.SourceSet, workbench.MetadataChangeRequest) {
	t.Helper()
	scope, bound := repositorySource("documents", "plan.md")
	bound.Spec.Writes = &apiv1.WorkbenchWrites{Fields: []apiv1.WorkbenchField{"title", "description"}}
	set := workbench.SourceSet{Scope: scope, Sources: []workbench.BoundSource{bound}}
	client := &metadataClient{sourceClient: sourceClient{commit: strings.Repeat("a", 40), files: map[string]string{"plan.md": objectiveSource}}}
	p, err := NewRepositoryProposer(set, "strategy", client)
	if err != nil {
		t.Fatal(err)
	}
	file := fixtureSourceFile("plan.md", client.commit, objectiveSource)
	digest := sha256.Sum256(file.Content)
	title := "Revised objective"
	request := workbench.MetadataChangeRequest{Path: "plan.md", Expected: workbench.MetadataRevision{Commit: file.Commit, BlobID: file.BlobID, ContentDigest: hex.EncodeToString(digest[:])}, Field: "title", Value: &title}
	return p, client, set, request
}

func TestMetadataAdapterReappliesTypedPreviewAndBindsEveryEffect(t *testing.T) {
	p, client, set, request := metadataAdapterFixture(t)
	// Construction copied target/allowlist/path and scope; a later config object
	// mutation cannot widen this already selected immutable applied snapshot.
	set.Sources[0].Spec.Paths[0] = "other.md"
	set.Sources[0].Spec.Repository.Name = "other"
	set.Sources[0].Spec.Writes.Fields[0] = "state"
	delete(set.Scope.Bindings, "strategy")
	preview, err := p.Preview(context.Background(), request)
	if err != nil || !preview.Changed || client.mutations != 0 || len(client.reads) != 1 {
		t.Fatal(preview, err)
	}
	proposal, err := p.Prepare(strings.Repeat("1", 32), request, preview)
	if err != nil || proposal.Repository.Name != "repo" || proposal.BaseBranch != "main" || proposal.PreviousBlob != request.Expected.BlobID || string(proposal.Content) != preview.After || !strings.Contains(proposal.Body, providers.RepositoryProposalMarker(proposal.CommandID, preview.OperationDigest)) {
		t.Fatal(proposal, err)
	}
	in := providers.RepositoryProposalPhaseInput{Phase: "tree", Proposal: proposal}
	result, err := p.Apply(context.Background(), request, preview, in)
	if err != nil || !result.Acknowledged || client.mutations != 1 {
		t.Fatal(result, err)
	}
	for _, tamper := range []string{"preview", "content", "target", "operation"} {
		badPreview, badInput := preview, in
		switch tamper {
		case "preview":
			badPreview.After = "arbitrary replacement"
		case "content":
			badInput.Proposal.Content = []byte("arbitrary replacement")
		case "target":
			badInput.Proposal.Repository.Name = "foreign"
		case "operation":
			badInput.Proposal.OperationDigest = strings.Repeat("f", 64)
		}
		if _, err := p.Apply(context.Background(), request, badPreview, badInput); err == nil || client.mutations != 1 {
			t.Fatal("unbound provider effect", tamper, err)
		}
	}
}

func TestMetadataAdapterRejectsStaleAndUnverifiableSourceBeforeEffects(t *testing.T) {
	for _, mode := range []string{"stale", "tamper", "path", "write removed"} {
		t.Run(mode, func(t *testing.T) {
			p, client, _, request := metadataAdapterFixture(t)
			switch mode {
			case "stale":
				client.commit = strings.Repeat("f", 40)
			case "tamper":
				client.tamper = true
			case "path":
				request.Path = "secret.md"
			case "write removed":
				p.set.Sources[0].Spec.Writes = nil
			}
			if _, err := p.Preview(context.Background(), request); err == nil || client.mutations != 0 {
				t.Fatal("invalid preview accepted", err)
			}
			if mode != "tamper" && len(client.reads) != 0 {
				t.Fatal("read despite preflight refusal")
			}
		})
	}
}

func TestMetadataAdapterObservationUsesExactRetainedTargetAfterWriteRevocation(t *testing.T) {
	p, client, _, request := metadataAdapterFixture(t)
	preview, err := p.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := p.Prepare(strings.Repeat("1", 32), request, preview)
	if err != nil {
		t.Fatal(err)
	}
	in := providers.RepositoryProposalPhaseInput{Phase: "tree", Proposal: proposal}
	p.set.Sources[0].Spec.Writes = nil
	if _, err := p.Apply(context.Background(), request, preview, in); !errors.Is(err, workbench.ErrMetadataEdit) || client.mutations != 0 {
		t.Fatal("revoked edit allowed", err)
	}
	if _, err := p.Observe(context.Background(), in); err != nil || client.observations != 1 {
		t.Fatal("read observation required old write policy", err)
	}
	in.Proposal.Repository.Name = "foreign"
	if _, err := p.Observe(context.Background(), in); err == nil || client.observations != 1 {
		t.Fatal("foreign observation", err)
	}
}
