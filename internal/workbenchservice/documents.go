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

// RepositoryFactory receives a validated declaration and callback-scoped human
// credential. The resulting client cannot be retained after the read returns.
type RepositoryFactory func(context.Context, ReadBinding, interactiveaccess.Credential) (workbenchprovider.RepositoryClient, error)

// Documents reads one bounded, commit-pinned window from an exact applied source
// under the same policy lease as repository credential selection and provider I/O.
func (s *Service) Documents(ctx context.Context, p httpapi.Principal, gaggle, binding string, request workbench.DocumentPageRequest) (workbench.DocumentPage, error) {
	if s == nil || s.Permissions == nil || s.Repository == nil {
		return workbench.DocumentPage{}, readError(http.StatusServiceUnavailable, "workbench_unavailable", "Repository source browsing is unavailable.")
	}
	ctx, cancel := context.WithTimeout(ctx, workbenchprovider.MaxReadDuration)
	defer cancel()
	var page workbench.DocumentPage
	err := s.Permissions.WithSourceRead(ctx, p, gaggle, "repository.read", func(ctx context.Context, g *apiv1.Gaggle, load interactiveaccess.SourceCredentialLoader) error {
		selected, err := selectRepository(g, binding)
		if err != nil {
			return err
		}
		credential, err := load(ctx, interactiveaccess.Target{Kind: "repository", Repository: *selected.Source.Spec.Repository})
		if err != nil {
			return err
		}
		client, err := s.Repository(ctx, selected, credential)
		if err != nil {
			return err
		}
		reader, err := workbenchprovider.NewRepositoryReader(selected.Scope, selected.Source, client)
		if err != nil {
			return err
		}
		page, err = reader.Page(ctx, request)
		return err
	})
	if err != nil {
		if errors.Is(err, workbenchprovider.ErrSourceChanged) {
			return workbench.DocumentPage{}, readError(http.StatusConflict, "workbench_source_changed", "The configured branch changed. Restart the source read.")
		}
		return workbench.DocumentPage{}, publicReadError(err)
	}
	return page, nil
}

func selectRepository(g *apiv1.Gaggle, binding string) (ReadBinding, error) {
	set, err := workbench.BindSources(*g)
	if err != nil {
		return ReadBinding{}, readError(http.StatusConflict, "workbench_invalid_sources", "Workbench sources are not valid in the applied configuration.")
	}
	for _, source := range set.Sources {
		if source.Spec.Name == binding && (source.Spec.Kind == "documents" || source.Spec.Kind == "relationships") {
			return ReadBinding{Scope: set.Scope, Source: source, Generation: configDigest(g)}, nil
		}
	}
	return ReadBinding{}, readError(http.StatusNotFound, "workbench_source_not_found", "No repository source with this binding is configured in the gaggle.")
}
