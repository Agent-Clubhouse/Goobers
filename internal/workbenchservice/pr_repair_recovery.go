package workbenchservice

import (
	"context"
	"net/http"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

// PRRepairRecoveryService reads retained command authority after its producer
// turn ends. It exposes no provider mutation or caller-selected target.
type PRRepairRecoveryService struct {
	Queue       *triggerqueue.Store
	Permissions *interactiveaccess.Service
	Client      PRRepairFactory
	Now         func() time.Time
}

// Get returns current authorized retained evidence without minting credentials.
func (s *PRRepairRecoveryService) Get(ctx context.Context, p httpapi.Principal, gaggle, id string) (sessioning.PRRepairCommandView, error) {
	return s.review(ctx, p, gaggle, id, false)
}

// Check observes only joined unknown effects; absence never proves non-application.
func (s *PRRepairRecoveryService) Check(ctx context.Context, p httpapi.Principal, gaggle, id string) (sessioning.PRRepairCommandView, error) {
	return s.review(ctx, p, gaggle, id, true)
}

func (s *PRRepairRecoveryService) review(ctx context.Context, p httpapi.Principal, gaggle, id string, check bool) (sessioning.PRRepairCommandView, error) {
	var result sessioning.PRRepairCommandView
	if s == nil || s.Queue == nil || s.Permissions == nil || s.Client == nil {
		return result, readError(503, "pr_repair_recovery_unavailable", "Repair observation is unavailable.")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := s.Permissions.WithPRRepairReview(ctx, p, gaggle, func(ctx context.Context, g *apiv1.Gaggle, access interactiveaccess.PRRepairReviewAccess) error {
		record, err := s.Queue.PRRepairCommandForReview(ctx, gaggle, id)
		if err != nil {
			return err
		}
		bound, load, err := repairReviewSource(g, record, access)
		if err != nil {
			return err
		}
		if check && record.State == "attempting" {
			return readError(http.StatusConflict, "pr_repair_attempt_unjoined", "The original provider attempt has no joined receipt; inspection cannot settle it.")
		}
		if check && record.State == "unknown" {
			record, err = s.observe(ctx, p, record, bound, load)
			if err != nil {
				return err
			}
		}
		result = prRepairView(record)
		return nil
	})
	if err != nil {
		return sessioning.PRRepairCommandView{}, prRepairError(err)
	}
	return result, nil
}

func repairReviewSource(g *apiv1.Gaggle, record triggerqueue.PRRepairCommand, access interactiveaccess.PRRepairReviewAccess) (ReadBinding, interactiveaccess.RepositoryCredentialLoader, error) {
	bound, err := selectRepository(g, record.Input.Scope.SourceBindingID)
	if err != nil {
		return ReadBinding{}, nil, err
	}
	r := bound.Source.Spec.Repository
	selected := sessioning.RepairRepository{Provider: string(r.Provider), Owner: r.Owner, Project: r.Project, Name: r.Name}
	if record.Input.Scope.Gaggle != g.Name || selected != record.Input.Selection.Repository {
		return ReadBinding{}, nil, interactiveaccess.ErrDenied
	}
	load, err := access(interactiveaccess.Target{Kind: "repository", Repository: *r})
	return bound, load, err
}

func (s *PRRepairRecoveryService) observe(ctx context.Context, p httpapi.Principal, record triggerqueue.PRRepairCommand, bound ReadBinding, load interactiveaccess.RepositoryCredentialLoader) (triggerqueue.PRRepairCommand, error) {
	native, err := record.Native()
	if err != nil {
		return record, err
	}
	credential, err := load(ctx)
	if err != nil {
		return record, err
	}
	client, err := s.Client(ctx, bound, credential)
	if err != nil {
		return record, err
	}
	observed, observationErr := client.ObservePullRequestRepair(ctx, native)
	// Any failed/moved/foreign/absent evidence remains unproven. Never persist
	// provider error prose, or a partial positive result returned with an error.
	if observationErr != nil {
		observed = providers.PRRepairObservation{}
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	return s.Queue.ObservePRRepairCommand(cleanup, record.Input.Scope, record.ID, record.RequestDigest, sessioning.Actor{Issuer: p.Issuer, Subject: p.Subject}, observed, now)
}
