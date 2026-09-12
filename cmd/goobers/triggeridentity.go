package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/localscheduler"
)

type acceptedPriorityTriggerer interface {
	TriggerPriorityWithDispatchRunID(context.Context, context.Context, localscheduler.WorkflowIdentity, string, time.Time, string) (string, error)
}

func dispatchAcceptedPriority(ctx, dispatchCtx context.Context, dispatch workflowTriggerer, request httpapi.TriggerRequest, now time.Time) (string, error) {
	priority, ok := dispatch.(acceptedPriorityTriggerer)
	if !ok {
		return "", errors.New("priority dispatcher does not support durable run identity")
	}
	return priority.TriggerPriorityWithDispatchRunID(ctx, dispatchCtx, localscheduler.WorkflowIdentity{Gaggle: request.Gaggle, Workflow: request.Workflow}, strings.TrimSpace(request.SourceRun), now, request.DispatchRunID)
}
