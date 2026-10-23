package control

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
)

type describeFunc func() (*workflowservice.DescribeWorkflowExecutionResponse, error)

func (d describeFunc) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return d()
}

func withStatus(status enums.WorkflowExecutionStatus) describeFunc {
	return func() (*workflowservice.DescribeWorkflowExecutionResponse, error) {
		return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: status}}, nil
	}
}

func TestTemporalRunClosedOnlyForEnumeratedClosedStatuses(t *testing.T) {
	outage := errors.New("temporal down")
	for name, tc := range map[string]struct {
		describe describeFunc
		closed   bool
		failed   bool
	}{
		"running":     {describe: withStatus(enums.WORKFLOW_EXECUTION_STATUS_RUNNING)},
		"completed":   {describe: withStatus(enums.WORKFLOW_EXECUTION_STATUS_COMPLETED), closed: true},
		"failed":      {describe: withStatus(enums.WORKFLOW_EXECUTION_STATUS_FAILED), closed: true},
		"terminated":  {describe: withStatus(enums.WORKFLOW_EXECUTION_STATUS_TERMINATED), closed: true},
		"canceled":    {describe: withStatus(enums.WORKFLOW_EXECUTION_STATUS_CANCELED), closed: true},
		"timed out":   {describe: withStatus(enums.WORKFLOW_EXECUTION_STATUS_TIMED_OUT), closed: true},
		"continued":   {describe: withStatus(enums.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW), closed: true},
		"paused":      {describe: withStatus(enums.WORKFLOW_EXECUTION_STATUS_PAUSED)},
		"unspecified": {describe: withStatus(enums.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED), failed: true},
		"unknown":     {describe: withStatus(enums.WorkflowExecutionStatus(99)), failed: true},
		"forgotten": {describe: func() (*workflowservice.DescribeWorkflowExecutionResponse, error) {
			return nil, serviceerror.NewNotFound("workflow not found")
		}, closed: true},
		"outage": {describe: func() (*workflowservice.DescribeWorkflowExecutionResponse, error) { return nil, outage }, failed: true},
		"incomplete": {describe: func() (*workflowservice.DescribeWorkflowExecutionResponse, error) {
			return &workflowservice.DescribeWorkflowExecutionResponse{}, nil
		}, failed: true},
	} {
		t.Run(name, func(t *testing.T) {
			closed, err := temporalRunClosed(tc.describe)(t.Context(), "run")
			if (err != nil) != tc.failed || closed != tc.closed {
				t.Fatalf("closed = %v, %v; want %v (error %v)", closed, err, tc.closed, tc.failed)
			}
		})
	}
}
