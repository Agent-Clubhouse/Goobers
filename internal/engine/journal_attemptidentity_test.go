package engine

import (
	"testing"

	"github.com/goobers/goobers/internal/attemptidentity"
	"github.com/goobers/goobers/internal/journal"
)

func TestAddAttemptIdentityIncludesKnownTemporalFields(t *testing.T) {
	ev := journal.Event{}
	addAttemptIdentity(&ev, &attemptidentity.Identity{
		BuildID:        "build-1",
		WorkerIdentity: "worker-1",
		TaskQueue:      "queue-1",
		ActivityID:     "activity-1",
		ActivityType:   "VersioningActivity",
		Attempt:        2,
	})

	if got := ev.Runner["buildId"]; got != "build-1" {
		t.Fatalf("runner.buildId = %v, want build-1", got)
	}
	if got := ev.Runner["workerIdentity"]; got != "worker-1" {
		t.Fatalf("runner.workerIdentity = %v, want worker-1", got)
	}
	if got := ev.Runner["taskQueue"]; got != "queue-1" {
		t.Fatalf("runner.taskQueue = %v, want queue-1", got)
	}
	if got := ev.Runner["activityId"]; got != "activity-1" {
		t.Fatalf("runner.activityId = %v, want activity-1", got)
	}
	if got := ev.Runner["activityType"]; got != "VersioningActivity" {
		t.Fatalf("runner.activityType = %v, want VersioningActivity", got)
	}
	if got := ev.Runner["activityAttempt"]; got != int32(2) {
		t.Fatalf("runner.activityAttempt = %v, want 2", got)
	}
}

func TestAddAttemptIdentityOmitsUnknownTemporalFields(t *testing.T) {
	ev := journal.Event{}
	addAttemptIdentity(&ev, &attemptidentity.Identity{
		BuildID:        "build-1",
		WorkerIdentity: "worker-1",
	})

	if _, ok := ev.Runner["taskQueue"]; ok {
		t.Fatalf("runner.taskQueue = %v, want omitted", ev.Runner["taskQueue"])
	}
	if _, ok := ev.Runner["activityId"]; ok {
		t.Fatalf("runner.activityId = %v, want omitted", ev.Runner["activityId"])
	}
	if _, ok := ev.Runner["activityType"]; ok {
		t.Fatalf("runner.activityType = %v, want omitted", ev.Runner["activityType"])
	}
	if _, ok := ev.Runner["activityAttempt"]; ok {
		t.Fatalf("runner.activityAttempt = %v, want omitted", ev.Runner["activityAttempt"])
	}
}
