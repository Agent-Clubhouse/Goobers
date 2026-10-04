// Package childmonitor projects bounded child custody for authorized humans.
package childmonitor

import (
	"context"
	"errors"
	"net/http"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

const pageSize = 50

// Service reads retained child lineage under the current human gaggle policy.
type Service struct {
	Layout      instance.Layout
	Queue       *triggerqueue.Store
	Permissions httpapi.InteractivePermissionService
	Scrubber    journal.Scrubber
}

// ListChildWorkflows returns a bounded page with only verified execution links.
func (s *Service) ListChildWorkflows(ctx context.Context, principal httpapi.Principal, run, after string) (apicontract.ChildWorkflowPage, error) {
	if s == nil || s.Queue == nil || s.Permissions == nil || s.Scrubber == nil {
		return apicontract.ChildWorkflowPage{}, errors.New("child monitor dependencies unavailable")
	}
	if !apiv1.ValidRunID(run) || len(after) > 128 {
		return apicontract.ChildWorkflowPage{}, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest, "Invalid run or child cursor.", nil)
	}
	id, err := s.authorizedIdentity(ctx, principal, run)
	if err != nil {
		return apicontract.ChildWorkflowPage{}, err
	}
	result := apicontract.ChildWorkflowPage{RunID: run, Gaggle: id.Gaggle, Children: []apicontract.ChildWorkflowSummary{}}
	if id.Child != nil {
		result.Parent = &apicontract.ChildWorkflowParent{RunID: id.Child.ParentRunID, Workflow: id.Child.ParentWorkflow, InvocationKey: s.scrub(id.Child.InvocationKey)}
	}
	children, err := s.Queue.Children(ctx, triggerqueue.ChildParent{Gaggle: id.Gaggle, ParentRunID: run}, after, pageSize+1)
	if err != nil {
		return result, err
	}
	if len(children) > pageSize {
		children = children[:pageSize]
		result.NextCursor = children[len(children)-1].ChildID
	}
	for _, child := range children {
		view, err := s.summarize(ctx, child)
		if err != nil {
			return apicontract.ChildWorkflowPage{}, err
		}
		result.Children = append(result.Children, view)
	}
	return result, nil
}

func (s *Service) authorizedIdentity(ctx context.Context, p httpapi.Principal, run string) (journal.RunIdentity, error) {
	missing := httpapi.NewInterventionError(http.StatusNotFound, "run_not_found", "Run is unavailable in your authorized gaggles.", nil)
	if p.Subject == "" || p.Issuer == "" || strings.HasPrefix(p.Issuer, "goobers/") || !p.HasRole(httpapi.RoleView) {
		return journal.RunIdentity{}, missing
	}
	id, err := s.identity(run)
	if err != nil {
		return journal.RunIdentity{}, missing
	}
	permissions, err := s.Permissions.InteractiveCapabilities(ctx, p, id.Gaggle)
	if err != nil || permissions.Gaggle != id.Gaggle || (permissions.PolicyConfigured && !permissions.Viewer) {
		return journal.RunIdentity{}, missing
	}
	if id.ValidateChildLineage() != nil {
		return journal.RunIdentity{}, errors.New("child monitor lineage is invalid")
	}
	return id, nil
}

func (s *Service) identity(run string) (journal.RunIdentity, error) {
	dir, err := s.Layout.FindRunDir(run)
	if err != nil {
		return journal.RunIdentity{}, err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return journal.RunIdentity{}, err
	}
	id, err := reader.Identity()
	if err == nil && id.RunID != run {
		err = errors.New("child monitor run identity differs")
	}
	return id, err
}

func (s *Service) summarize(ctx context.Context, child triggerqueue.ChildRecord) (apicontract.ChildWorkflowSummary, error) {
	view := apicontract.ChildWorkflowSummary{ChildID: child.ChildID, RunID: child.RunID, InvocationKey: s.scrub(child.Identity.InvocationKey), Sequence: child.Sequence, State: string(child.State), CancellationRequested: child.CancellationRequested, Acknowledged: !child.AcknowledgedAt.IsZero(), Expired: !child.TombstonedAt.IsZero(), AcceptedAt: child.AcceptedAt, UpdatedAt: child.UpdatedAt}
	if view.Expired {
		return view, nil
	}
	start, err := s.Queue.ChildStart(ctx, child.Identity)
	if err != nil {
		return view, err
	}
	envelope, err := childworkflow.DecodeStartEnvelope(start.Payload)
	if err != nil || envelope.Identity() != child.Identity || start.ID != child.AcceptanceID || envelope.SourceDigest != child.ProposalDigest {
		return view, errors.New("child monitor retained start differs from lineage")
	}
	view.Stage, view.Workflow = envelope.ParentStage, envelope.Workflow
	if id, err := s.identity(child.RunID); err == nil && id.Child != nil {
		lineage := id.Child
		view.RunAvailable = id.ValidateChildLineage() == nil && id.Gaggle == child.Identity.Gaggle && lineage.ParentRunID == child.Identity.ParentRunID && lineage.StageOccurrence == child.Identity.StageOccurrence && lineage.InvocationKey == child.Identity.InvocationKey && lineage.AcceptanceID == child.AcceptanceID && lineage.EnvelopeDigest == journal.Digest(start.Payload)
	}
	return view, nil
}

func (s *Service) scrub(value string) string { return string(s.Scrubber.Scrub([]byte(value))) }
