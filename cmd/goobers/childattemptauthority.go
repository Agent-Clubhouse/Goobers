package main

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

type childAttemptCustody struct {
	contract childpod.Contract
	digest   string
	blobs    childpod.ChildAttemptBlobs
	reader   *journal.Reader
	review   bool
	service  *daemonCredentialService
}

// The signed contract chooses one physical attempt; request body identities
// and a guessed digest never widen it to a sibling stage or ordinary run.
func (s *daemonCredentialService) childAttempt(ctx context.Context) (childAttemptCustody, error) {
	p, ok := httpapi.PrincipalFromContext(ctx)
	if !ok || p.Issuer != httpapi.GeneratedChildPrincipalIssuer || p.GeneratedChild == nil || !blobstore.ValidDigest(p.GeneratedChild.ContractDigest) {
		return childAttemptCustody{}, childworkflow.ErrAuthorityUnavailable
	}
	identity, scoped, err := s.childBlobScope(ctx)
	if err != nil || !scoped {
		return childAttemptCustody{}, errors.Join(childworkflow.ErrAuthorityUnavailable, err)
	}
	blobs := childpod.ChildAttemptBlobs{Store: childpod.ScopedBlobs{Queue: s.childQueue, Identity: identity}, ContractDigest: p.GeneratedChild.ContractDigest}
	raw, err := blobs.Get(ctx, p.GeneratedChild.ContractDigest)
	if err != nil {
		return childAttemptCustody{}, err
	}
	contract, err := childpod.DecodeContract(raw, p.GeneratedChild.ContractDigest)
	if err != nil {
		return childAttemptCustody{}, err
	}
	dir, err := s.layout.FindRunDir(strings.TrimPrefix(p.Subject, "run:"))
	if err != nil {
		return childAttemptCustody{}, err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return childAttemptCustody{}, err
	}
	id, err := reader.Identity()
	if err != nil {
		return childAttemptCustody{}, err
	}
	if contract.Identity.Child == nil || !reflect.DeepEqual(contract.Identity, id) {
		return childAttemptCustody{}, errors.New("child contract differs from retained journal")
	}
	review, err := childContractReview(reader, contract)
	if err != nil {
		return childAttemptCustody{}, err
	}
	return childAttemptCustody{contract: contract, digest: p.GeneratedChild.ContractDigest, blobs: blobs, reader: reader, review: review, service: s}, nil
}

// Review authority is derived only from the immutable, trusted workflow input.
func childContractReview(reader *journal.Reader, contract childpod.Contract) (bool, error) {
	machine, err := runner.PinnedWorkflowMachine(reader, contract.Identity)
	if err != nil {
		return false, err
	}
	if _, ok := machine.Task(contract.Stage); ok {
		return false, nil
	}
	if gate, ok := machine.Gate(contract.Stage); ok && gate.Agentic != nil {
		return true, nil
	}
	return false, errors.New("child contract stage has no executable task or reviewer")
}

func (a childAttemptCustody) active(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	event, err := childPodStarted(a.reader, a.contract.Stage, a.contract.Attempt, a.review)
	if err != nil {
		return err
	}
	if event.Seq != uint64(a.contract.PodAttempt) || !event.Time.Equal(a.contract.StartedAt) {
		return childworkflow.ErrAuthorityChanged
	}
	return nil
}

// Custody allows only the original unresolved writer to finish surrendering.
// It never authorizes credentials, a new dispatch or child tool authority.
func (a childAttemptCustody) custody(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pending, err := a.service.childPodCustodyPending(ctx, a.reader, a.digest)
	if err != nil {
		return err
	}
	if !pending {
		return childworkflow.ErrAuthorityUnavailable
	}
	return nil
}

type generatedBlobPlane struct {
	base    blobstore.Store
	service *daemonCredentialService
}

func (p generatedBlobPlane) Describe() string { return "generated-attempt-blob-plane" }
func (p generatedBlobPlane) store(ctx context.Context) (blobstore.Store, error) {
	principal, ok := httpapi.PrincipalFromContext(ctx)
	if !ok || principal.Issuer != httpapi.GeneratedChildPrincipalIssuer {
		return p.base, nil
	}
	a, err := p.service.childAttempt(ctx)
	if err != nil {
		return nil, err
	}
	if err = a.custody(ctx); err != nil {
		return nil, err
	}
	return a.blobs, nil
}
func (p generatedBlobPlane) Get(ctx context.Context, digest string) ([]byte, error) {
	s, err := p.store(ctx)
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, digest)
}
func (p generatedBlobPlane) Put(ctx context.Context, digest string, data []byte) error {
	s, err := p.store(ctx)
	if err != nil {
		return err
	}
	return s.Put(ctx, digest, data)
}
func (p generatedBlobPlane) Has(ctx context.Context, digest string) (bool, error) {
	_, err := p.Get(ctx, digest)
	if errors.Is(err, blobstore.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}
