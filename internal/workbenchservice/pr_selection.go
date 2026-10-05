package workbenchservice

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/providers"
)

// PRSelectionService reads a human-chosen native PR locator under current source
// authority. Its result is immutable message content, never an execution grant.
type PRSelectionService struct {
	Permissions *interactiveaccess.Service
	Client      PRRepairFactory
	Scrubber    journal.Scrubber
}

// Inspect selects the configured repository and its exact interactive credential.
// The operation only reads; current repair authority and host custody are checked
// independently when an accepted session turn attempts a repair.
func (s *PRSelectionService) Inspect(ctx context.Context, p httpapi.Principal, gaggle, binding, id string) (sessioning.PRRepairInspection, error) {
	var result sessioning.PRRepairInspection
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != id {
		return result, readError(http.StatusBadRequest, "invalid_request", "Choose a valid pull request number.")
	}
	if s == nil || s.Permissions == nil || s.Client == nil || s.Scrubber == nil {
		return result, readError(http.StatusServiceUnavailable, "pr_selection_unavailable", "Pull request inspection is unavailable.")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = s.Permissions.WithSourceRead(ctx, p, gaggle, "repository.read", func(ctx context.Context, g *apiv1.Gaggle, load interactiveaccess.SourceCredentialLoader) error {
		selected, err := selectRepository(g, binding)
		if err != nil {
			return err
		}
		target := *selected.Source.Spec.Repository
		credential, err := load(ctx, interactiveaccess.Target{Kind: "repository", Repository: target})
		if err != nil {
			return err
		}
		client, err := s.Client(ctx, selected, credential)
		if err != nil {
			return err
		}
		repo := providers.RepositoryRef{Provider: providers.ProviderKind(target.Provider), Owner: target.Owner, Project: target.Project, Name: target.Name}
		value, err := client.InspectRepairPullRequest(ctx, repo, id)
		if err != nil {
			return err
		}
		result, err = s.selection(binding, repo, id, value)
		return err
	})
	if err != nil {
		return sessioning.PRRepairInspection{}, prRepairError(err)
	}
	return result, nil
}

func (s *PRSelectionService) selection(binding string, repo providers.RepositoryRef, id string, value providers.RepairPullRequest) (sessioning.PRRepairInspection, error) {
	if value.Repository != repo || value.ID != id || !providers.ValidSourceCommit(value.BaseSHA) || len(value.Head) > 1024 || len(value.Base) > 1024 || value.Head == "" || value.Base == "" || value.Head == value.Base || len(value.Title) > 8192 || len(value.Body) > 64<<10 || len(value.URL) > 2048 {
		return sessioning.PRRepairInspection{}, providers.ErrPRRepair
	}
	selection := sessioning.PRRepairTarget{SourceBindingID: binding, Repository: sessioning.RepairRepository{Provider: string(repo.Provider), Owner: repo.Owner, Project: repo.Project, Name: repo.Name}, RepositorySourceID: value.RepositoryID, ID: id, SourceID: value.StableID, ExpectedHeadSHA: value.HeadSHA}
	if err := sessioning.ValidatePRRepairTarget(&selection); err != nil {
		return sessioning.PRRepairInspection{}, providers.ErrPRRepair
	}
	value.Title = string(s.Scrubber.Scrub([]byte(value.Title)))
	value.Body = string(s.Scrubber.Scrub([]byte(value.Body)))
	raw, err := json.Marshal(value)
	if err != nil || !bytes.Equal(raw, s.Scrubber.Scrub(raw)) {
		return sessioning.PRRepairInspection{}, providers.ErrPRRepair
	}
	return sessioning.PRRepairInspection{Target: selection, HeadSHA: value.HeadSHA, BaseSHA: value.BaseSHA, Head: value.Head, Base: value.Base, Title: value.Title, Description: value.Body, URL: value.URL, Open: value.Open, Draft: value.Draft}, nil
}
