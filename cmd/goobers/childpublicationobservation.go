package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childmonitor"
	"github.com/goobers/goobers/internal/childpublication"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func (u *upSession) childPublicationObservation() childmonitor.PublicationObservation {
	return func(ctx context.Context, p httpapi.Principal, id journal.RunIdentity, target childpublication.ObservationTarget, use func(context.Context, childpublication.EffectObserver, *journal.Run, []journal.Event) error) error {
		if target.SourceRunID != id.RunID {
			return httpapi.NewInterventionError(http.StatusConflict, "publication_execution_changed", "Check publication through the execution that created it.", nil)
		}
		repository := target.Repository
		selected := interactiveaccess.Target{Kind: "repository", Repository: apiv1.InteractiveRepositoryIdentity{Provider: apiv1.Provider(repository.Provider), Owner: repository.Owner, Project: repository.Project, Name: repository.Name}}
		err := u.setup.InteractiveAccess.WithRunObservationCredential(ctx, p, id.Gaggle, selected, func(ctx context.Context, credential interactiveaccess.Credential) error {
			observer, err := humanPublicationObserver(repository, credential)
			if err != nil {
				return err
			}
			release, ok := u.setup.RunnerRegistry.acquireChildCustody(id.RunID)
			if !ok {
				return publicationObservationBusy()
			}
			defer release()
			dir, err := u.l.FindRunDir(id.RunID)
			if err != nil {
				return err
			}
			info, err := os.Stat(filepath.Join(dir, "events.jsonl"))
			if err != nil {
				return err
			}
			if info.Size() > 32<<20 {
				return httpapi.NewInterventionError(http.StatusConflict, "interactive_history_too_large", "This run exceeds the interactive history read budget.", nil)
			}
			writer, report, err := journal.TryRecover(dir, journal.WithScrubber(journal.Chain(u.setup.SharedRegistry, journal.NewPatternScrubber())))
			if errors.Is(err, journal.ErrRecoveryBusy) {
				return publicationObservationBusy()
			}
			if err != nil {
				return err
			}
			defer func() { _ = writer.Close() }()
			reader, err := journal.OpenReadOnly(dir)
			if err != nil {
				return err
			}
			actual, err := reader.Identity()
			if err != nil || !reflect.DeepEqual(actual, id) || !publicationObservationStopped(writer.Phase()) {
				return publicationObservationBusy()
			}
			return use(ctx, observer, writer, report.Events)
		})
		if errors.Is(err, interactiveaccess.ErrDenied) || errors.Is(err, interactiveaccess.ErrCredentialUnavailable) {
			return httpapi.NewInterventionError(http.StatusForbidden, "interactive_access_denied", "The gaggle does not authorize this publication observation or its repository credential.", nil)
		}
		return err
	}
}

func publicationObservationStopped(phase journal.RunPhase) bool {
	switch phase {
	case journal.PhaseCompleted, journal.PhaseFailed, journal.PhaseAborted, journal.PhaseEscalated:
		return true
	default:
		return false
	}
}

func publicationObservationBusy() error {
	return httpapi.NewInterventionError(http.StatusConflict, "publication_execution_active", "The child execution still owns its journal. Check again after it stops.", nil)
}

// This interface exposes only provider reads and uses the gaggle's selected
// interactive identity. It never inherits the child's automation credentials.
func humanPublicationObserver(repo providers.RepositoryRef, credential interactiveaccess.Credential) (childpublication.EffectObserver, error) {
	if credential.Value == "" || (!credential.ExpiresAt.IsZero() && !credential.ExpiresAt.After(time.Now())) {
		return nil, interactiveaccess.ErrCredentialUnavailable
	}
	switch repo.Provider {
	case providers.ProviderGitHub:
		if credential.Scheme != "bearer" || (repo.URL != "" && repo.URL != "https://github.com") {
			return nil, interactiveaccess.ErrCredentialUnavailable
		}
		return providers.NewGitHubProvider(credential.Value), nil
	case providers.ProviderADO:
		if repo.URL != "" && repo.URL != "https://dev.azure.com" {
			return nil, interactiveaccess.ErrCredentialUnavailable
		}
		kind := providers.ADOCredentialKindBearer
		if credential.Scheme == "basic" {
			kind = providers.ADOCredentialKindPAT
		} else if credential.Scheme != "bearer" {
			return nil, interactiveaccess.ErrCredentialUnavailable
		}
		source, err := providers.NewADODeliveredCredentialSourceWithExpiry(kind, credential.Value, "interactive publication observation", credential.ExpiresAt)
		if err != nil {
			return nil, err
		}
		return providers.NewADOProvider(repo.Owner, repo.Project, "", providers.WithADOCredentialSource(source)), nil
	default:
		return nil, interactiveaccess.ErrCredentialUnavailable
	}
}
