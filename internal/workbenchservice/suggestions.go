package workbenchservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchsuggestions"
)

// SuggestionService explicitly inspects retained artifacts and hands reviewed
// relationships to the ordinary metadata proposal service. It scans no runs and
// neither trusts artifact authority nor automatically changes sources.
type SuggestionService struct {
	Proposals *ProposalService
	OpenRun   func(context.Context, string, string) (*journal.Reader, error)
}

type suggestionSnapshot struct {
	set        workbench.SourceSet
	generation string
	access     interactiveaccess.ProposalSnapshotAccess
}

func (s *SuggestionService) withSnapshot(ctx context.Context, p httpapi.Principal, gaggle string, use func(context.Context, suggestionSnapshot) error) error {
	return s.snapshot(ctx, p, gaggle, false, use)
}
func (s *SuggestionService) snapshot(ctx context.Context, p httpapi.Principal, gaggle string, decision bool, use func(context.Context, suggestionSnapshot) error) error {
	if s == nil || s.Proposals == nil || s.Proposals.ReadService == nil || s.Proposals.ReadService.Permissions == nil || s.Proposals.Queue == nil || s.Proposals.Provider == nil || s.OpenRun == nil {
		return readError(http.StatusServiceUnavailable, "workbench_suggestions_unavailable", "Relationship suggestion review is unavailable.")
	}
	callback := s.Proposals.ReadService.Permissions.WithSourceProposalSnapshot
	if decision {
		callback = s.Proposals.ReadService.Permissions.WithSourceReviewDecision
	}
	err := callback(ctx, p, gaggle, func(ctx context.Context, g *apiv1.Gaggle, access interactiveaccess.ProposalSnapshotAccess) error {
		set, err := workbench.BindSources(*g)
		if err != nil {
			return readError(http.StatusConflict, "workbench_invalid_sources", "Workbench sources are not valid in the applied configuration.")
		}
		return use(ctx, suggestionSnapshot{set: set, generation: configDigest(g), access: access})
	})
	return suggestionError(err)
}

// Artifacts lists a bounded window of provenance under current gaggle visibility.
func (s *SuggestionService) Artifacts(ctx context.Context, p httpapi.Principal, gaggle, runID string, after uint64) (workbench.SuggestionInventory, error) {
	var result workbench.SuggestionInventory
	err := s.withSnapshot(ctx, p, gaggle, func(ctx context.Context, _ suggestionSnapshot) error {
		rd, err := s.OpenRun(ctx, gaggle, runID)
		if err != nil {
			return err
		}
		result, err = workbenchsuggestions.List(ctx, rd, gaggle, runID, after)
		return err
	})
	if err != nil {
		return workbench.SuggestionInventory{}, err
	}
	return result, nil
}

// Load returns only candidates whose endpoints are currently visible. Read
// permission never grants acceptance; support flags describe operation shape.
func (s *SuggestionService) Load(ctx context.Context, p httpapi.Principal, gaggle string, selection workbench.SuggestionSelection) (workbench.SuggestionBatch, error) {
	var result workbench.SuggestionBatch
	err := s.withSnapshot(ctx, p, gaggle, func(ctx context.Context, snap suggestionSnapshot) error {
		loaded, err := s.loadArtifact(ctx, snap, selection)
		if err != nil {
			return err
		}
		result = workbench.SuggestionBatch{Selection: selection, Artifact: loaded.Artifact, Candidates: []workbench.SuggestionCandidate{}}
		for _, bound := range loaded.Suggestions {
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = snap.authorizeSuggestion(bound); err != nil {
				result.Omitted++
				continue
			}
			candidate := workbench.SuggestionCandidate{Suggestion: bound, Reason: suggestionUnsupported(bound)}
			candidate.Supported = candidate.Reason == ""
			prior, err := s.Proposals.Queue.FindWorkbenchSuggestion(ctx, suggestionScope(p, gaggle), bound.Key)
			if err == nil {
				candidate.ReviewID, candidate.ReviewState = prior.ID, prior.State
			} else if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, triggerqueue.ErrWorkbenchCommandExpired) {
				return err
			}
			result.Candidates = append(result.Candidates, candidate)
		}
		return nil
	})
	if err != nil {
		return workbench.SuggestionBatch{}, err
	}
	return result, nil
}

func (s *SuggestionService) loadArtifact(ctx context.Context, snap suggestionSnapshot, selection workbench.SuggestionSelection) (workbenchsuggestions.Loaded, error) {
	rd, err := s.OpenRun(ctx, snap.set.Scope.GaggleID, selection.RunID)
	if err != nil {
		return workbenchsuggestions.Loaded{}, err
	}
	return workbenchsuggestions.Load(ctx, rd, snap.set, selection)
}
func (snap suggestionSnapshot) source(binding string) (ReadBinding, error) {
	for _, source := range snap.set.Sources {
		if source.Spec.Name == binding {
			return ReadBinding{Scope: snap.set.Scope, Source: source, Generation: snap.generation}, nil
		}
	}
	return ReadBinding{}, workbench.ErrSuggestion
}
func (snap suggestionSnapshot) readAccess(binding string) (ReadBinding, interactiveaccess.RepositoryCredentialLoader, error) {
	bound, err := snap.source(binding)
	if err != nil {
		return bound, nil, err
	}
	action, target := apiv1.InteractiveAction("backlog.read"), interactiveaccess.Target{Kind: "backlog"}
	if bound.Source.Spec.Kind != "backlog" {
		action, target = "repository.read", interactiveaccess.Target{Kind: "repository", Repository: *bound.Source.Spec.Repository}
	}
	load, err := snap.access(action, target)
	return bound, load, err
}
func (snap suggestionSnapshot) authorizeSuggestion(bound workbench.BoundSuggestion) error {
	if !workbench.MatchesSuggestionKey(bound) {
		return workbench.ErrSuggestion
	}
	raw, err := json.Marshal(workbench.RelationshipSuggestions{SchemaVersion: workbench.SuggestionSchemaVersion, Suggestions: []workbench.RelationshipSuggestion{bound.Proposal}})
	if err != nil {
		return err
	}
	if _, err = workbench.ParseSuggestions(raw, snap.set); err != nil {
		return err
	}
	for _, endpoint := range []workbench.SuggestionEndpoint{bound.Proposal.From, bound.Proposal.To} {
		binding := ""
		if endpoint.Ref != nil {
			binding = endpoint.Ref.SourceBindingID
		} else if endpoint.Creation != nil {
			binding = endpoint.Creation.SourceBindingID
		}
		if _, _, err = snap.readAccess(binding); err != nil {
			return err
		}
	}
	return nil
}
func suggestionUnsupported(bound workbench.BoundSuggestion) string {
	if bound.Proposal.Kind != "references" && bound.Proposal.Kind != "contributes-to" {
		return "native-relationship-not-supported"
	}
	for _, e := range []workbench.SuggestionEndpoint{bound.Proposal.From, bound.Proposal.To} {
		if e.Creation != nil {
			return "requires-confirmed-creation"
		}
		if e.Ref == nil || (e.Ref.Kind != "work-item" && e.Ref.Kind != "objective-document") {
			return "endpoint-kind-not-supported"
		}
	}
	return ""
}
func suggestionScope(p httpapi.Principal, gaggle string) triggerqueue.WorkbenchSuggestionScope {
	return triggerqueue.WorkbenchSuggestionScope{Gaggle: gaggle, Actor: sessioning.Actor{Issuer: p.Issuer, Subject: p.Subject}}
}
func suggestionError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, workbench.ErrSuggestion) || errors.Is(err, workbenchsuggestions.ErrArtifact) {
		return readError(http.StatusConflict, "workbench_suggestion_changed", "The retained suggestion or configured source evidence cannot be verified.")
	}
	return proposalError(err)
}
