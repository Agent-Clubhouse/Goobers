package control

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"

	"github.com/goobers/goobers/internal/cloud/acceptance"
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
		halts    bool
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
		"unavailable": {describe: func() (*workflowservice.DescribeWorkflowExecutionResponse, error) {
			return nil, serviceerror.NewUnavailable("connection refused")
		}, failed: true, halts: true},
		"deadline": {describe: func() (*workflowservice.DescribeWorkflowExecutionResponse, error) {
			return nil, serviceerror.NewDeadlineExceeded("deadline exceeded")
		}, failed: true, halts: true},
		"context deadline": {describe: func() (*workflowservice.DescribeWorkflowExecutionResponse, error) {
			return nil, context.DeadlineExceeded
		}, failed: true, halts: true},
		"incomplete": {describe: func() (*workflowservice.DescribeWorkflowExecutionResponse, error) {
			return &workflowservice.DescribeWorkflowExecutionResponse{}, nil
		}, failed: true},
	} {
		t.Run(name, func(t *testing.T) {
			closed, err := temporalRunClosed(tc.describe)(t.Context(), "run")
			if (err != nil) != tc.failed || closed != tc.closed || errors.Is(err, acceptance.ErrRunStatusUnavailable) != tc.halts {
				t.Fatalf("closed = %v, %v; want %v (error %v, halts %v)", closed, err, tc.closed, tc.failed, tc.halts)
			}
		})
	}
}

func TestRunPinReportsOncePerUnconfirmedRunAndOutage(t *testing.T) {
	var reported []string
	report := func(err error) { reported = append(reported, err.Error()) }
	stuck, outage := errors.New("stuck"), errors.New("outage")
	reports := runPinReports{}
	for _, sweep := range []acceptance.RunPinSweep{
		{Unconfirmed: map[string]error{"a": stuck}},
		{Unconfirmed: map[string]error{"a": stuck}, Halted: outage},
		{Unconfirmed: map[string]error{"a": stuck}, Halted: outage},
		{Checked: []string{"a"}},
		{Unconfirmed: map[string]error{"a": stuck}, Halted: outage},
	} {
		reports.report(sweep, report)
	}
	// "a" and the outage are each reported when they start, not while they
	// persist, and again only after they cleared in between.
	if got, want := fmt.Sprint(reported), "[stuck outage stuck outage]"; got != want {
		t.Fatalf("reported %s, want %s", got, want)
	}
}
