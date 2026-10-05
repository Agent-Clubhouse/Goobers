package main

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/httpapi"
)

type parentBlobPlane struct {
	base    blobstore.Store
	service *daemonCredentialService
}

func (p parentBlobPlane) Describe() string { return "contained-parent-blob-plane" }
func (p parentBlobPlane) store(ctx context.Context) (blobstore.Store, error) {
	principal, ok := httpapi.PrincipalFromContext(ctx)
	if !ok || !principal.WorkflowParent {
		return p.base, nil
	}
	a, err := p.service.parentAttempt(ctx)
	if err != nil {
		return nil, err
	}
	if err = a.custody(ctx); err != nil {
		return nil, err
	}
	return a.blobs, nil
}
func (p parentBlobPlane) Get(ctx context.Context, digest string) ([]byte, error) {
	s, err := p.store(ctx)
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, digest)
}
func (p parentBlobPlane) Put(ctx context.Context, digest string, data []byte) error {
	s, err := p.store(ctx)
	if err != nil {
		return err
	}
	return s.Put(ctx, digest, data)
}
func (p parentBlobPlane) Has(ctx context.Context, digest string) (bool, error) {
	_, err := p.Get(ctx, digest)
	if errors.Is(err, blobstore.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}
