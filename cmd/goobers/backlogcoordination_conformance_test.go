package main

import (
	"strings"
	"testing"
)

// TestBacklogCoordinationConformance proves COORD-6's assignment-aware
// coordination contract against the same hermetic provider fixture used by
// the backlog-query and backlog-assignment command tests.
func TestBacklogCoordinationConformance(t *testing.T) {
	t.Run("round robin partitions a shared backlog by identity", func(t *testing.T) {
		root, server := assignmentCommandFixture(t)
		for number := 2; number <= 8; number++ {
			server.addIssue(number, "Ready item", "goobers:approved", "goobers:ready")
		}
		t.Setenv("GOOBERS_INPUT_STRATEGY", assignmentStrategyRoundRobin)
		t.Setenv("GOOBERS_INPUT_ROSTER", `[{"assignee":"identity-a"},{"assignee":"identity-b"}]`)

		code, stdout, stderr := runArgs(t, "backlog-assignment", root)
		if code != 0 {
			t.Fatalf("backlog-assignment: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if got := strings.Join(fakeIssueAssignees(server, 1, 2, 3, 4, 5, 6, 7, 8), ","); got != "identity-a,identity-b,identity-a,identity-b,identity-a,identity-b,identity-a,identity-b" {
			t.Fatalf("round-robin assignees = %q, want an even four/four split", got)
		}

		server.addIssue(9, "Still unassigned", "goobers:approved", "goobers:ready")
		if got := coordinationEligibleIDs(t, root, "identity-a"); got != "1,3,5,7" {
			t.Fatalf("identity A eligible IDs = %q, want only its assigned items", got)
		}
		if got := coordinationEligibleIDs(t, root, "identity-b"); got != "2,4,6,8" {
			t.Fatalf("identity B eligible IDs = %q, want only its assigned items", got)
		}
	})

	t.Run("constant cap leaves overflow unassigned without error", func(t *testing.T) {
		root, server := assignmentCommandFixture(t)
		setFakeIssueAssignee(server, 1, "identity-a")
		for number := 2; number <= 6; number++ {
			server.addIssue(number, "Ready item", "goobers:approved", "goobers:ready")
		}
		t.Setenv("GOOBERS_INPUT_STRATEGY", assignmentStrategyConstantCap)
		t.Setenv("GOOBERS_INPUT_ROSTER", `[{"assignee":"identity-a","maxOpen":3}]`)

		code, stdout, stderr := runArgs(t, "backlog-assignment", root)
		if code != 0 {
			t.Fatalf("backlog-assignment overflow: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
		}
		if got := strings.Join(fakeIssueAssignees(server, 1, 2, 3, 4, 5, 6), ","); got != "identity-a,identity-a,identity-a,,," {
			t.Fatalf("constant-cap assignees = %q, want exactly three assigned and overflow untouched", got)
		}
		report := readAssignmentReport(t)
		if len(report.Assignments) != 2 || report.Unassigned != 3 || report.NoWork {
			t.Fatalf("constant-cap report = %+v, want two new assignments and three unassigned", report)
		}
	})
}

func coordinationEligibleIDs(t *testing.T, root, identity string) string {
	t.Helper()
	t.Setenv("GOOBERS_INPUT_RESPECTASSIGNEE", "true")
	t.Setenv("GOOBERS_INPUT_ASSIGNEDTO", identity)

	code, stdout, stderr := runArgs(t, "backlog-query", root)
	if code != 0 {
		t.Fatalf("backlog-query as %s: code = %d, stdout = %q, stderr = %q", identity, code, stdout, stderr)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if id, _, ok := strings.Cut(line, "\t"); ok {
			ids = append(ids, id)
		}
	}
	return strings.Join(ids, ",")
}
