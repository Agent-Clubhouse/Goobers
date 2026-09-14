package localscheduler

import (
	"testing"
	"time"
)

// The daemon heartbeat polls WorkflowCount on every tick so a hot reload is
// reflected in operator-facing output. Before #5075 the count was captured at
// startup, so a healthy reload that added workflows kept printing the old
// number and read as incomplete.
func TestWorkflowCountFollowsReload(t *testing.T) {
	entries := []WorkflowEntry{
		{Gaggle: "g", Workflow: "one"},
		{Gaggle: "g", Workflow: "two"},
		{Gaggle: "g", Workflow: "three"},
	}
	s, _ := newTestScheduler(t, entries)
	if got := s.WorkflowCount(); got != 3 {
		t.Fatalf("initial WorkflowCount = %d, want 3", got)
	}

	grown := append(append([]WorkflowEntry{}, entries...),
		WorkflowEntry{Gaggle: "g", Workflow: "four"},
		WorkflowEntry{Gaggle: "g", Workflow: "five"},
	)
	if err := s.Reload(grown, nil, time.Now(), "sha256:old", "sha256:new"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := s.WorkflowCount(); got != 5 {
		t.Fatalf("WorkflowCount after reload = %d, want 5", got)
	}

	// A reload that removes workflows must move the count down too.
	if err := s.Reload(entries[:1], nil, time.Now(), "sha256:new", "sha256:newer"); err != nil {
		t.Fatalf("Reload shrink: %v", err)
	}
	if got := s.WorkflowCount(); got != 1 {
		t.Fatalf("WorkflowCount after shrinking reload = %d, want 1", got)
	}
}
