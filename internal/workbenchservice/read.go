// Package workbenchservice serves source-owned planning data under the current
// interactive policy. It stores no planning truth and never borrows automation
// credentials. Providers and shared-session callers use the same projections.
package workbenchservice

import (
	"context"
	"errors"
	"net/http"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
)

// ReadBinding contains only validated scope and the applied configuration digest.
// It contains neither caller-selected endpoints nor credentials.
type ReadBinding struct {
	Scope      workbench.Scope
	Source     workbench.BoundSource
	Generation string
}

// BacklogFactory receives callback-scoped interactive credentials. Its client
// must complete all requests within that callback and cannot be retained.
type BacklogFactory func(context.Context, ReadBinding, interactiveaccess.Credential) (workbenchprovider.BacklogClient, error)

// Service supplies current-policy source browsing and live-session adapters.
type Service struct {
	Permissions *interactiveaccess.Service
	Backlog     BacklogFactory
	Repository  RepositoryFactory
}

// Get reads one current native locator under the gaggle source policy.
func (s *Service) Get(ctx context.Context, p httpapi.Principal, gaggle, binding string, request workbench.BacklogItemRequest) (workbench.BacklogItem, error) {
	var result workbench.BacklogItem
	err := s.read(ctx, p, gaggle, binding, func(ctx context.Context, r *workbenchprovider.BacklogReader) error {
		var err error
		result, err = r.GetWithRelationships(ctx, request)
		return err
	})
	return result, err
}

// Page reads one bounded native provider window under current authorization.
func (s *Service) Page(ctx context.Context, p httpapi.Principal, gaggle, binding string, request workbench.BacklogPageRequest) (workbench.BacklogPage, error) {
	var result workbench.BacklogPage
	err := s.read(ctx, p, gaggle, binding, func(ctx context.Context, r *workbenchprovider.BacklogReader) error {
		var err error
		result, err = r.Page(ctx, request)
		return err
	})
	return result, err
}
func (s *Service) read(ctx context.Context, p httpapi.Principal, gaggle, binding string, use func(context.Context, *workbenchprovider.BacklogReader) error) error {
	if s == nil || s.Permissions == nil || s.Backlog == nil {
		return readError(http.StatusServiceUnavailable, "workbench_unavailable", "Backlog browsing is unavailable.")
	}
	ctx, cancel := context.WithTimeout(ctx, workbenchprovider.MaxReadDuration)
	defer cancel()
	err := s.Permissions.WithSourceRead(ctx, p, gaggle, "backlog.read", func(ctx context.Context, g *apiv1.Gaggle, load interactiveaccess.SourceCredentialLoader) error {
		selected, err := selectBacklog(g, binding)
		if err != nil {
			return err
		}
		credential, err := load(ctx, interactiveaccess.Target{Kind: "backlog"})
		if err != nil {
			return err
		}
		return s.useBacklog(ctx, selected, credential, use)
	})
	return publicReadError(err)
}
func selectBacklog(g *apiv1.Gaggle, binding string) (ReadBinding, error) {
	set, err := workbench.BindSources(*g)
	if err != nil {
		return ReadBinding{}, readError(http.StatusConflict, "workbench_invalid_sources", "Workbench sources are not valid in the applied configuration.")
	}
	for _, source := range set.Sources {
		if source.Spec.Name == binding && source.Spec.Kind == "backlog" {
			return ReadBinding{Scope: set.Scope, Source: source, Generation: configDigest(g)}, nil
		}
	}
	return ReadBinding{}, readError(http.StatusNotFound, "workbench_source_not_found", "No backlog source with this binding is configured in the gaggle.")
}
func (s *Service) useBacklog(ctx context.Context, binding ReadBinding, credential interactiveaccess.Credential, use func(context.Context, *workbenchprovider.BacklogReader) error) error {
	client, err := s.Backlog(ctx, binding, credential)
	if err != nil {
		return err
	}
	reader, err := workbenchprovider.NewBacklogReader(binding.Scope, binding.Source, client)
	if err != nil {
		return err
	}
	return use(ctx, reader)
}
func readError(status int, code, message string) error {
	return &httpapi.InterventionError{Status: status, Code: code, Message: message}
}
func publicReadError(err error) error {
	if err == nil {
		return nil
	}
	var public *httpapi.InterventionError
	if errors.As(err, &public) {
		return public
	}
	switch {
	case errors.Is(err, interactiveaccess.ErrDenied):
		return readError(http.StatusForbidden, "interactive_access_denied", "Current gaggle permission does not allow this source read.")
	case errors.Is(err, interactiveaccess.ErrCredentialUnavailable):
		return readError(http.StatusServiceUnavailable, "interactive_credential_unavailable", "The configured interactive source credential is unavailable.")
	case errors.Is(err, workbenchprovider.ErrInvalidCursor):
		return readError(http.StatusBadRequest, "invalid_request", "The page cursor does not match this source and page size.")
	case errors.Is(err, workbenchprovider.ErrIdentityChanged):
		return readError(http.StatusConflict, "workbench_identity_changed", "This locator no longer identifies the expected source item.")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return readError(http.StatusGatewayTimeout, "workbench_read_interrupted", "The bounded source read did not complete.")
	default:
		return readError(http.StatusBadGateway, "workbench_source_read_failed", "The source could not return a valid bounded projection.")
	}
}
