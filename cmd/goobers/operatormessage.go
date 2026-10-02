package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

const operatorMessageDeniedCode = "not_authorized"

func (s *daemonRunJournalService) SubmitOperatorMessage(ctx context.Context, request httpapi.OperatorMessageSubmissionRequest) (httpapi.OperatorMessageSubmissionResponse, error) {
	if err := ctx.Err(); err != nil {
		return httpapi.OperatorMessageSubmissionResponse{}, err
	}
	if !apiv1.ValidRunID(request.RunID) || strings.TrimSpace(request.IdempotencyKey) == "" ||
		strings.TrimSpace(request.PrincipalRef) == "" {
		return httpapi.OperatorMessageSubmissionResponse{}, httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest,
			"runId, idempotency key, and principal are required", nil)
	}
	message := apiv1.OperatorMessageRequest{
		Schema:         apiv1.OperatorMessageRequestSchema,
		RequestID:      request.IdempotencyKey,
		IdempotencyKey: request.IdempotencyKey,
		TargetAddress:  request.TargetAddress,
		PrincipalRef:   request.PrincipalRef,
		RequestedAt:    time.Now().UTC(),
		ExpiresAt:      request.ExpiresAt,
		Purpose:        request.Purpose,
		Content:        request.Content,
		DeliveryMode:   request.DeliveryMode,
	}
	if !s.runJournalGaggleOK(request.Gaggle, request.RunID) {
		if err := s.recordOperatorMessageTargetScopeDenied(request.RunID, message); err != nil {
			s.journalOperatorMessageTargetScopeError(request.RunID, err)
		}
		return httpapi.OperatorMessageSubmissionResponse{}, gaggleMismatch("operator message submission")
	}
	if !operatorMessagePrincipalCanSubmit(request.Principal, request.RunID) {
		record, _, err := s.recordOperatorMessageDenied(request.Gaggle, request.RunID, message)
		if err != nil {
			return httpapi.OperatorMessageSubmissionResponse{}, err
		}
		return httpapi.OperatorMessageSubmissionResponse{Accepted: false, Record: record}, httpapi.NewInterventionError(http.StatusForbidden,
			operatorMessageDeniedCode, "principal is not authorized to submit operator messages for this run", nil)
	}

	record, accepted, err := s.recordOperatorMessageAccepted(request.Gaggle, request.RunID, message)
	if err != nil {
		return httpapi.OperatorMessageSubmissionResponse{}, err
	}
	return apicontract.OperatorMessageSubmitResponse{Accepted: accepted, Record: record}, nil
}

func operatorMessagePrincipalCanSubmit(principal httpapi.Principal, runID string) bool {
	if httpapi.IsPodPrincipal(principal) {
		return principal.Subject == "run:"+runID && principal.HasScope(httpapi.ScopeJournal)
	}
	return principal.HasRole(httpapi.RoleOperate)
}

func (s *daemonRunJournalService) recoverOperatorMessageRun(gaggle, runID string) (*journal.Run, error) {
	run, _, err := journal.Recover(filepath.Join(s.layout.ForGaggle(gaggle).RunsDir(), runID))
	return run, err
}

func (s *daemonRunJournalService) recordOperatorMessageAccepted(gaggle, runID string, request apiv1.OperatorMessageRequest) (apiv1.OperatorMessageRecord, bool, error) {
	run, err := s.recoverOperatorMessageRun(gaggle, runID)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, false, err
	}
	defer func() { _ = run.Close() }()
	return run.AcceptOperatorMessage(request)
}

func (s *daemonRunJournalService) recordOperatorMessageDenied(gaggle, runID string, request apiv1.OperatorMessageRequest) (apiv1.OperatorMessageRecord, bool, error) {
	run, err := s.recoverOperatorMessageRun(gaggle, runID)
	if err != nil {
		return apiv1.OperatorMessageRecord{}, false, err
	}
	defer func() { _ = run.Close() }()
	return run.RejectOperatorMessage(request, operatorMessageDeniedCode, "principal is not authorized to submit operator messages for this run")
}

func (s *daemonRunJournalService) recordOperatorMessageTargetScopeDenied(runID string, request apiv1.OperatorMessageRequest) error {
	gaggle, ok, err := s.uniqueGaggleForRun(runID)
	if err != nil || !ok {
		return err
	}
	_, _, err = s.recordOperatorMessageDenied(gaggle, runID, request)
	return err
}

func (s *daemonRunJournalService) journalOperatorMessageTargetScopeError(runID string, err error) {
	if s.log == nil || err == nil {
		return
	}
	s.log.AppendBestEffort(journal.Event{
		Type:   journal.EventError,
		RunID:  runID,
		Reason: "operator-message-target-scope-denial-journal-failed",
		Error:  journal.ErrorDetailFor("operator_message_denial_journal_failed", err),
	})
}

func (s *daemonRunJournalService) uniqueGaggleForRun(runID string) (string, bool, error) {
	entries, err := os.ReadDir(s.layout.GagglesDir())
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var found string
	for _, entry := range entries {
		if !entry.IsDir() || !plainPathElement(entry.Name()) {
			continue
		}
		_, err := os.Stat(filepath.Join(s.layout.ForGaggle(entry.Name()).RunsDir(), runID, "run.yaml"))
		switch {
		case err == nil:
			if found != "" {
				return "", false, nil
			}
			found = entry.Name()
		case errors.Is(err, os.ErrNotExist):
		default:
			return "", false, err
		}
	}
	return found, found != "", nil
}

var _ httpapi.OperatorMessageService = (*daemonRunJournalService)(nil)

func withDaemonRunJournalServices(layout instance.Layout, log *journal.InstanceLog) []httpapi.HandlerOption {
	service := newDaemonRunJournalService(layout, log)
	return []httpapi.HandlerOption{
		httpapi.WithRunJournalService(service),
		httpapi.WithOperatorMessageService(service),
	}
}
