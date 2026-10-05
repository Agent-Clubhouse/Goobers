package main

import (
	"errors"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func restartWaitingReason(err error) triggerqueue.WaitingReason {
	if errors.Is(err, interactiveaccess.ErrDenied) || errors.Is(err, interactiveaccess.ErrCredentialUnavailable) {
		return triggerqueue.WaitingAccess
	}
	var scheduler *localscheduler.TriggerRejectedError
	if errors.As(err, &scheduler) && scheduler.Transient() {
		return triggerqueue.WaitingCapacity
	}
	var apiError *httpapi.InterventionError
	if !errors.As(err, &apiError) {
		return triggerqueue.WaitingValidation
	}
	switch apiError.Code {
	case "run_not_admitted", "scheduler_unavailable":
		return triggerqueue.WaitingCapacity
	case "restart_source_changed", "restart_execution_changed", "restart_workspace_retained", "restart_source_missing", "restart_backlog_changed", "restart_repository_changed", "restart_branch_unverified", "restart_recovery_retained":
		return triggerqueue.WaitingSource
	case "restart_source_active", "intervention_in_progress", "run_owner_changed", "restart_recovery_uncertain":
		return triggerqueue.WaitingSourceBusy
	case "restart_execution_unavailable", "restart_preflight_unavailable", "interactive_engine_unsupported", "restart_workspace_unavailable", "restart_shared_claim", "restart_pr_scope_ambiguous":
		return triggerqueue.WaitingUnsupported
	case "restart_gaggle_disabled", "restart_workflow_disabled":
		return triggerqueue.WaitingAccess
	default:
		if apiError.Status == 401 || apiError.Status == 403 {
			return triggerqueue.WaitingAccess
		}
		return triggerqueue.WaitingValidation
	}
}

func restartPendingReason(record triggerqueue.Record) string {
	switch record.State {
	case triggerqueue.Accepted:
		return record.Reason
	case triggerqueue.Dispatching:
		return "Waiting for execution confirmation; this request will not be resent."
	default:
		return ""
	}
}
