package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
)

// fakeQueueDescriber answers DescribeTaskQueue from a table keyed by
// "<queue>/<type>".
type fakeQueueDescriber struct {
	pollers map[string][]string
	err     error
	asked   []string
}

func (f *fakeQueueDescriber) DescribeTaskQueue(_ context.Context, queue string, kind enumspb.TaskQueueType) (*workflowservice.DescribeTaskQueueResponse, error) {
	key := queue + "/" + strings.ToLower(strings.TrimPrefix(kind.String(), "TaskQueueType"))
	f.asked = append(f.asked, key)
	if f.err != nil {
		return nil, f.err
	}
	var infos []*taskqueuepb.PollerInfo
	for _, identity := range f.pollers[key] {
		infos = append(infos, &taskqueuepb.PollerInfo{Identity: identity})
	}
	return &workflowservice.DescribeTaskQueueResponse{Pollers: infos}, nil
}

// The queue-ownership evidence (decision 003 far-side item 11): every queue is
// described for BOTH task types, and each poller's identity is reported. Both
// types matter — the activity type is the one whose poller runs DispatchStage
// and therefore creates pods, and the workflow type is what carries
// DispatchOne, so a check that looked at only one could read an empty answer
// off a correctly served queue.
func TestDescribeEngineQueuesCoversBothTaskTypes(t *testing.T) {
	describer := &fakeQueueDescriber{pollers: map[string][]string{
		"goobers-engine/workflow":                 {"goobers-worker/v1@goobers-worker-0#1"},
		"goobers-engine/activity":                 {"goobers-worker/v1@goobers-worker-0#1"},
		"goobers-dispatch.e2e.linux-pod/activity": {"goobers-worker/v1@goobers-worker-0#1"},
		"goobers-dispatch.e2e.linux-pod/workflow": {"goobers-worker/v1@goobers-worker-0#1"},
	}}
	rows, err := describeEngineQueues(context.Background(), describer, "goobers-engine",
		[]string{"goobers-dispatch.e2e.linux-pod"})
	if err != nil {
		t.Fatalf("describeEngineQueues: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want one per (queue x task type): %+v", len(rows), rows)
	}
	byKey := map[string]queuePollers{}
	for _, row := range rows {
		byKey[row.Queue+"/"+row.Type] = row
	}
	for _, key := range []string{
		"goobers-engine/workflow", "goobers-engine/activity",
		"goobers-dispatch.e2e.linux-pod/workflow", "goobers-dispatch.e2e.linux-pod/activity",
	} {
		row, ok := byKey[key]
		if !ok {
			t.Fatalf("no row for %s; described %v", key, describer.asked)
		}
		if len(row.Pollers) != 1 || !strings.HasPrefix(row.Pollers[0], "goobers-worker/") {
			t.Fatalf("%s pollers = %v, want the worker's identity", key, row.Pollers)
		}
	}
	// The dispatch marker lets a checker select the goobers-dispatch.* set
	// without re-deriving the prefix.
	if byKey["goobers-engine/workflow"].Dispatch {
		t.Fatal("the workflow queue is marked as a dispatch queue")
	}
	if !byKey["goobers-dispatch.e2e.linux-pod/activity"].Dispatch {
		t.Fatal("a goobers-dispatch.* queue is not marked as one")
	}
}

// The finding the cluster check most needs to read is "nobody polls this" —
// and, on the negative-control side, "goobers-api polls nothing". An empty
// poller list must survive to the report and print explicitly.
func TestDescribeEngineQueuesReportsUnpolledQueue(t *testing.T) {
	rows, err := describeEngineQueues(context.Background(), &fakeQueueDescriber{}, "goobers-engine", nil)
	if err != nil {
		t.Fatalf("describeEngineQueues: %v", err)
	}
	for _, row := range rows {
		if len(row.Pollers) != 0 {
			t.Fatalf("%s/%s reported pollers %v against an empty frontend", row.Queue, row.Type, row.Pollers)
		}
	}
	var out bytes.Buffer
	printEngineQueues(&out, rows)
	if !strings.Contains(out.String(), "(none)") {
		t.Fatalf("report %q does not say a queue has no pollers", out.String())
	}
}

// A describe that fails is an error, not an empty answer: a check that read
// "no pollers" off an unreachable frontend would assert the daemon polls
// nothing and be right for the wrong reason.
func TestDescribeEngineQueuesFailsLoudly(t *testing.T) {
	_, err := describeEngineQueues(context.Background(),
		&fakeQueueDescriber{err: errors.New("frontend unavailable")}, "goobers-engine", nil)
	if err == nil {
		t.Fatal("a failed describe was reported as an empty poller list")
	}
	if !strings.Contains(err.Error(), "goobers-engine") {
		t.Fatalf("error %q does not name the queue it could not describe", err)
	}
}
