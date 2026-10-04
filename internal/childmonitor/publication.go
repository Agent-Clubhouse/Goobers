package childmonitor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childpublication"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// PublicationObservation holds current interactive credentials and exclusive
// journal custody for one bounded, read-only provider observation callback.
// The host verifies identity and a stopped execution before supplying the writer.
type PublicationObservation func(context.Context, httpapi.Principal, journal.RunIdentity, childpublication.ObservationTarget, func(context.Context, childpublication.EffectObserver, *journal.Run, []journal.Event) error) error

func publicationView(status childpublication.Status) apicontract.ChildPublicationSummary {
	view := apicontract.ChildPublicationSummary{Action: string(status.Action), IntentDigest: status.IntentDigest, State: status.State, Head: status.Head, Base: status.Base, Commit: status.Commit, PullRequestURL: status.PullRequestURL, PullRequestNumber: status.PullRequestNumber, NeedsHuman: status.NeedsHuman, CreatedAt: status.CreatedAt, UpdatedAt: status.UpdatedAt, Observation: status.Observation}
	if !status.CheckedAt.IsZero() {
		view.CheckedAt = &status.CheckedAt
	}
	return view
}

func (s *Service) publications(ctx context.Context, p httpapi.Principal, id journal.RunIdentity, page *apicontract.ChildWorkflowPage) error {
	if id.Child == nil {
		return nil
	}
	child, err := s.publicationChild(ctx, id)
	if err != nil {
		return err
	}
	if !child.TombstonedAt.IsZero() {
		return nil
	}
	if err = s.executionHistory(ctx, child, page); err != nil {
		return err
	}
	statuses, err := childpublication.Inspect(ctx, s.Queue, child.Identity)
	if err != nil {
		return err
	}
	for _, status := range statuses {
		page.Publications = append(page.Publications, publicationView(status))
	}
	if len(statuses) == 0 {
		return nil
	}
	page.PublicationRunID = child.RunID
	permissions, err := s.Permissions.InteractiveCapabilities(ctx, p, id.Gaggle)
	if err != nil {
		return err
	}
	intervention, read := false, false
	for _, action := range permissions.Actions {
		if action.Action == "run.intervene" {
			intervention = action.Authorized
		}
		if action.Action == "repository.read" {
			read = action.Authorized && action.CredentialConfigured
		}
	}
	page.PublicationCheckAvailable = s.Observe != nil && permissions.Operator && intervention && read
	if !page.PublicationCheckAvailable {
		page.PublicationCheckReason = "Checking publication requires run intervention and repository read access with this gaggle’s configured interactive credential."
	}
	return nil
}

// CheckChildPublication records the authenticated human in the existing durable
// operator-message ledger. Same-key retries return the recorded observation;
// a later observation uses a fresh request key and never creates another effect.
func (s *Service) CheckChildPublication(ctx context.Context, p httpapi.Principal, run, key string, input apicontract.ChildPublicationCheckRequest) (apicontract.ChildPublicationCheckResult, error) {
	var result apicontract.ChildPublicationCheckResult
	if s == nil || s.Queue == nil || s.Permissions == nil || s.Scrubber == nil {
		return result, errors.New("child monitor dependencies unavailable")
	}
	action := childpublication.Action(input.Action)
	if strings.TrimSpace(key) == "" || len(key) > 200 || !action.Valid() || !blobstore.ValidDigest(input.ExpectedIntentDigest) {
		return result, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest, "Invalid publication check.", nil)
	}
	id, err := s.authorizedIdentity(ctx, p, run)
	if err != nil {
		return result, err
	}
	if id.Child == nil || s.Observe == nil || !p.HasRole(httpapi.RoleOperate) {
		return result, httpapi.NewInterventionError(http.StatusForbidden, "publication_check_unavailable", "Publication checks are unavailable for this run.", nil)
	}
	child, err := s.publicationChild(ctx, id)
	if err != nil {
		return result, err
	}
	target, err := childpublication.InspectTarget(ctx, s.Queue, child.Identity, action, input.ExpectedIntentDigest)
	if err != nil {
		return result, publicationConflict()
	}
	if target.ChildRunID != run || target.ParentRunID != id.Child.ParentRunID {
		return result, publicationConflict()
	}
	ctx, cancel := context.WithTimeout(ctx, childpublication.EffectTimeout)
	defer cancel()
	err = s.Observe(ctx, p, id, target, func(ctx context.Context, observer childpublication.EffectObserver, writer *journal.Run, events []journal.Event) error {
		var callErr error
		result, callErr = s.checkPublicationOwned(ctx, p, child, key, input, observer, writer, events)
		return callErr
	})
	return result, err
}

// A valid journal shape alone does not establish custody of a retained effect.
// Bind all public identity fields to the original queue envelope before reading
// publication details or selecting a repository credential.
func (s *Service) publicationChild(ctx context.Context, id journal.RunIdentity) (triggerqueue.ChildRecord, error) {
	child, err := s.Queue.ChildForExecutionRun(ctx, id.RunID)
	if err != nil {
		return child, err
	}
	if id.Child == nil || id.ValidateChildLineage() != nil || id.Gaggle != child.Identity.Gaggle {
		return child, publicationConflict()
	}
	if !child.TombstonedAt.IsZero() {
		return child, nil
	}
	start, err := s.Queue.ChildStart(ctx, child.Identity)
	if err != nil {
		return child, err
	}
	envelope, err := childworkflow.DecodeStartEnvelope(start.Payload)
	if err != nil || envelope.Identity() != child.Identity || start.ID != child.AcceptanceID || envelope.SourceDigest != child.ProposalDigest {
		return child, publicationConflict()
	}
	expected := journal.ChildLineage{Gaggle: child.Identity.Gaggle, ParentRunID: child.Identity.ParentRunID, ParentWorkflow: envelope.ParentWorkflow, StageOccurrence: child.Identity.StageOccurrence, InvocationKey: child.Identity.InvocationKey, AcceptanceID: child.AcceptanceID, SourceDigest: child.ProposalDigest, EnvelopeDigest: journal.Digest(start.Payload)}
	if id.Child.ExecutionEpoch > 0 {
		execution, err := s.Queue.ChildExecutionMetadata(ctx, child.Identity, id.RunID)
		if err != nil || execution.Epoch != id.Child.ExecutionEpoch || execution.SourceRunID != id.ContinuedFromRunID || execution.SourceTerminalSeq != id.SourceTerminalSeq || execution.Actor != id.Operator || execution.Stage != id.RequestedTarget {
			return child, publicationConflict()
		}
		expected.ExecutionEpoch, expected.PriorResultRef, expected.RestartDigest = execution.Epoch, execution.SourceResultRef, execution.RequestDigest
	}
	if *id.Child != expected || id.Workflow != envelope.Workflow || id.WorkflowDigest != envelope.WorkflowDigest || id.GooberDigest != envelope.ParentGooberDigest || id.ConfigGeneration != envelope.ConfigGeneration {
		return child, publicationConflict()
	}
	return child, nil
}

func publicationConflict() error {
	return httpapi.NewInterventionError(http.StatusConflict, "publication_intent_changed", "The recorded publication is unavailable or differs from this request. Refresh before checking again.", nil)
}

func (s *Service) checkPublicationOwned(ctx context.Context, p httpapi.Principal, child triggerqueue.ChildRecord, key string, input apicontract.ChildPublicationCheckRequest, observer childpublication.EffectObserver, writer *journal.Run, events []journal.Event) (apicontract.ChildPublicationCheckResult, error) {
	var zero apicontract.ChildPublicationCheckResult
	if writer == nil || observer == nil {
		return zero, errors.New("publication observation dependencies unavailable")
	}
	request := publicationRequest(p, child, key, input)
	records := journal.ReplayOperatorMessages(events)
	var record apiv1.OperatorMessageRecord
	count := 0
	for _, prior := range records {
		if prior.Request.DeliveryMode == "publication-observation" {
			count++
		}
		if prior.Request.IdempotencyKey != request.IdempotencyKey {
			continue
		}
		if prior.Request.PrincipalRef != request.PrincipalRef || prior.Request.TargetAddress != request.TargetAddress || prior.Request.DeliveryMode != request.DeliveryMode || prior.Request.Content.Text != request.Content.Text {
			return zero, publicationConflict()
		}
		record = prior
	}
	if record.Request.RequestID == "" {
		if count >= 100 {
			return zero, httpapi.NewInterventionError(http.StatusConflict, "publication_check_limit", "This run has reached its retained publication check limit.", nil)
		}
		var err error
		record, _, err = writer.AcceptOperatorMessage(request)
		if err != nil {
			return zero, err
		}
	}
	if record.Outcome != nil {
		return replayPublicationCheck(writer.Dir(), record, input, child.RunID)
	}
	status, err := (childpublication.Reconciler{Queue: s.Queue, Observer: observer}).Check(ctx, child.Identity, childpublication.Action(input.Action), input.ExpectedIntentDigest)
	if err != nil {
		return zero, httpapi.NewInterventionError(http.StatusServiceUnavailable, "publication_observation_failed", "The publication could not be observed. Retry the same check.", nil)
	}
	result := apicontract.ChildPublicationCheckResult{RunID: child.RunID, RequestID: record.Request.RequestID, Publication: publicationView(status)}
	raw, err := json.Marshal(result)
	if err != nil {
		return zero, err
	}
	ref, err := writer.RecordArtifactBoundedWithIntegrity("publication-check/"+record.Request.RequestID, raw, apiv1.IntegrityDerived, 16<<10)
	if err != nil {
		return zero, err
	}
	detail, err := json.Marshal(ref)
	if err != nil {
		return zero, err
	}
	_, err = writer.CompleteOperatorMessage(apiv1.OperatorMessageOutcome{Schema: apiv1.OperatorMessageOutcomeSchema, RequestID: record.Request.RequestID, IdempotencyKey: record.Request.IdempotencyKey, CompletedAt: time.Now().UTC(), Status: apiv1.OperatorMessageDelivered, Code: "publication_observed", Detail: string(detail)})
	return result, err
}

func publicationRequest(p httpapi.Principal, child triggerqueue.ChildRecord, key string, input apicontract.ChildPublicationCheckRequest) apiv1.OperatorMessageRequest {
	digest := sha256.Sum256([]byte(p.Issuer + "\x00" + p.Subject + "\x00" + child.Identity.Gaggle + "\x00" + key))
	id := fmt.Sprintf("publication-check-%x", digest)
	text, _ := json.Marshal(input)
	return apiv1.OperatorMessageRequest{Schema: apiv1.OperatorMessageRequestSchema, RequestID: id, IdempotencyKey: fmt.Sprintf("human:%x", digest), TargetAddress: "child-publication:" + input.Action + "@" + input.ExpectedIntentDigest, PrincipalRef: p.Issuer + ":" + p.Subject, RequestedAt: time.Now().UTC(), Purpose: "reconcile-child-publication", DeliveryMode: "publication-observation", Content: apiv1.OperatorMessageContent{Text: string(text)}}
}

func replayPublicationCheck(dir string, record apiv1.OperatorMessageRecord, input apicontract.ChildPublicationCheckRequest, run string) (apicontract.ChildPublicationCheckResult, error) {
	var result apicontract.ChildPublicationCheckResult
	var ref journal.Ref
	if record.Outcome.Status != apiv1.OperatorMessageDelivered || record.Outcome.Code != "publication_observed" || json.Unmarshal([]byte(record.Outcome.Detail), &ref) != nil || ref.Size <= 0 || ref.Size > 16<<10 {
		return result, publicationConflict()
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return result, err
	}
	raw, err := reader.ArtifactBytes(ref)
	if err != nil {
		return result, err
	}
	if json.Unmarshal(raw, &result) != nil || result.RequestID != record.Request.RequestID || result.RunID != run || result.Publication.Action != input.Action || result.Publication.IntentDigest != input.ExpectedIntentDigest {
		return apicontract.ChildPublicationCheckResult{}, publicationConflict()
	}
	return result, nil
}
