package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestBacklogAssignmentConstantCapStopsAtCeiling(t *testing.T) {
	root, server := assignmentCommandFixture(t)
	setFakeIssueAssignee(server, 1, "alice")
	for number := 2; number <= 5; number++ {
		server.addIssue(number, "Ready item", "goobers:approved", "goobers:ready")
	}
	t.Setenv("GOOBERS_INPUT_STRATEGY", assignmentStrategyConstantCap)
	t.Setenv("GOOBERS_INPUT_ROSTER", `[{"assignee":"alice","maxOpen":3}]`)

	code, stdout, stderr := runArgs(t, "backlog-assignment", root)
	if code != 0 {
		t.Fatalf("backlog-assignment: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got := fakeIssueAssignees(server, 1, 2, 3, 4, 5); strings.Join(got, ",") != "alice,alice,alice,," {
		t.Fatalf("assignees = %v, want cap at three with remainder unassigned", got)
	}
	report := readAssignmentReport(t)
	if len(report.Assignments) != 2 || report.Unassigned != 2 {
		t.Fatalf("report = %+v, want two assignments and two remaining", report)
	}
}

// TestPlanBacklogAssignmentsDisplayNameRosterCountsOpenItems is a regression
// guard for #5556: a roster entry configured with an Azure DevOps display
// name must keep counting an item whose provider-reported Assignee is that
// same display name — the working, unchanged behavior #5556 explicitly
// preserves — even though WorkItem now also carries an AssigneeAliases
// uniqueName alongside it.
func TestPlanBacklogAssignmentsDisplayNameRosterCountsOpenItems(t *testing.T) {
	roster := []assignmentRosterEntry{{Assignee: "Alex Example", MaxOpen: 1}}
	scoped := []providers.WorkItem{
		{ID: "1", Assignee: "Alex Example", AssigneeAliases: []string{"alex@example.com"}},
	}
	eligible := []providers.WorkItem{
		{ID: "2", Assignee: ""},
	}
	plan := planBacklogAssignments(assignmentStrategyConstantCap, roster, eligible, scoped, defaultAssignmentMaxItems)
	if len(plan) != 0 {
		t.Fatalf("plan = %+v, want no assignments: the display-name roster's one open item already fills maxOpen=1", plan)
	}
}

// TestPlanBacklogAssignmentsUniqueNameRosterCountsDisplayNameItem is the
// regression test for #5556: a roster entry configured with an Azure
// DevOps account's stable uniqueName must count an item whose
// provider-reported Assignee is that account's display name, matching via
// the item's AssigneeAliases. Before AssigneeMatches, planBacklogAssignments
// indexed load purely by an exact-string map lookup on item.Assignee, so a
// uniqueName-configured roster entry never matched a display-name-carrying
// item and load stayed at zero — over-assigning past maxOpen.
func TestPlanBacklogAssignmentsUniqueNameRosterCountsDisplayNameItem(t *testing.T) {
	roster := []assignmentRosterEntry{{Assignee: "alex@example.com", MaxOpen: 1}}
	scoped := []providers.WorkItem{
		{ID: "1", Assignee: "Alex Example", AssigneeAliases: []string{"alex@example.com"}},
	}
	eligible := []providers.WorkItem{
		{ID: "2", Assignee: ""},
	}
	plan := planBacklogAssignments(assignmentStrategyConstantCap, roster, eligible, scoped, defaultAssignmentMaxItems)
	if len(plan) != 0 {
		t.Fatalf("plan = %+v, want no assignments: the uniqueName roster's one open item (matched via AssigneeAliases) already fills maxOpen=1", plan)
	}
}

func TestBacklogAssignmentRoundRobinBalancesWithRosterTieOrder(t *testing.T) {
	root, server := assignmentCommandFixture(t)
	for number := 2; number <= 6; number++ {
		server.addIssue(number, "Ready item", "goobers:approved", "goobers:ready")
	}
	t.Setenv("GOOBERS_INPUT_STRATEGY", assignmentStrategyRoundRobin)
	t.Setenv("GOOBERS_INPUT_ROSTER", `[{"assignee":"alice"},{"assignee":"bob"}]`)

	code, stdout, stderr := runArgs(t, "backlog-assignment", root)
	if code != 0 {
		t.Fatalf("backlog-assignment: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	got := fakeIssueAssignees(server, 1, 2, 3, 4, 5, 6)
	want := "alice,bob,alice,bob,alice,bob"
	if strings.Join(got, ",") != want {
		t.Fatalf("assignees = %v, want deterministic roster-order split %s", got, want)
	}
}

func TestBacklogAssignmentConstantCapCountsExcludedOpenAssignments(t *testing.T) {
	root, server := assignmentCommandFixture(t)
	setFakeIssueLabels(server, 1, "goobers:approved", "goobers:ready", "goobers/status:in-review")
	setFakeIssueAssignee(server, 1, "alice")
	server.addIssue(2, "Ready item", "goobers:approved", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_STRATEGY", assignmentStrategyConstantCap)
	t.Setenv("GOOBERS_INPUT_ROSTER", `[{"assignee":"alice","maxOpen":1}]`)

	code, stdout, stderr := runArgs(t, "backlog-assignment", root)
	if code != 0 {
		t.Fatalf("backlog-assignment: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got := fakeIssueAssignees(server, 1, 2); strings.Join(got, ",") != "alice," {
		t.Fatalf("assignees = %v, want excluded open assignment to consume capacity", got)
	}
}

func TestBacklogAssignmentRoundRobinCountsExcludedOpenAssignments(t *testing.T) {
	root, server := assignmentCommandFixture(t)
	setFakeIssueLabels(server, 1, "goobers:approved", "goobers:ready", "goobers/status:in-review")
	setFakeIssueAssignee(server, 1, "alice")
	server.addIssue(2, "Ready item", "goobers:approved", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_STRATEGY", assignmentStrategyRoundRobin)
	t.Setenv("GOOBERS_INPUT_ROSTER", `[{"assignee":"alice"},{"assignee":"bob"}]`)

	code, stdout, stderr := runArgs(t, "backlog-assignment", root)
	if code != 0 {
		t.Fatalf("backlog-assignment: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got := fakeIssueAssignees(server, 1, 2); strings.Join(got, ",") != "alice,bob" {
		t.Fatalf("assignees = %v, want assignment to least-loaded bob", got)
	}
}

func TestBacklogAssignmentConstantCapCountsUntrustedOpenAssignments(t *testing.T) {
	root, server := assignmentCommandFixture(t)
	setFakeIssueLabels(server, 1, "goobers:ready")
	setFakeIssueAssignee(server, 1, "alice")
	server.addIssue(2, "Ready item", "goobers:approved", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_STRATEGY", assignmentStrategyConstantCap)
	t.Setenv("GOOBERS_INPUT_ROSTER", `[{"assignee":"alice","maxOpen":1}]`)

	code, stdout, stderr := runArgs(t, "backlog-assignment", root)
	if code != 0 {
		t.Fatalf("backlog-assignment: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got := fakeIssueAssignees(server, 1, 2); strings.Join(got, ",") != "alice," {
		t.Fatalf("assignees = %v, want untrusted open assignment to consume capacity", got)
	}
}

func TestBacklogAssignmentRoundRobinCountsUntrustedOpenAssignments(t *testing.T) {
	root, server := assignmentCommandFixture(t)
	setFakeIssueLabels(server, 1, "goobers:ready")
	setFakeIssueAssignee(server, 1, "alice")
	server.addIssue(2, "Ready item", "goobers:approved", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_STRATEGY", assignmentStrategyRoundRobin)
	t.Setenv("GOOBERS_INPUT_ROSTER", `[{"assignee":"alice"},{"assignee":"bob"}]`)

	code, stdout, stderr := runArgs(t, "backlog-assignment", root)
	if code != 0 {
		t.Fatalf("backlog-assignment: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if got := fakeIssueAssignees(server, 1, 2); strings.Join(got, ",") != "alice,bob" {
		t.Fatalf("assignees = %v, want assignment to least-loaded bob", got)
	}
}

func TestBacklogAssignmentSkipsConcurrentlyAssignedItem(t *testing.T) {
	root, server := assignmentCommandFixture(t)
	t.Setenv("GOOBERS_INPUT_STRATEGY", assignmentStrategyRoundRobin)
	t.Setenv("GOOBERS_INPUT_ROSTER", `[{"assignee":"alice"}]`)

	var stdout, stderr bytes.Buffer
	code := runBacklogAssignmentWithMutationHook([]string{root}, &stdout, &stderr, func(assignment assignmentPlanEntry) {
		setFakeIssueAssignee(server, 1, "human")
	})
	if code != 0 {
		t.Fatalf("backlog-assignment: code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if got := fakeIssueAssignees(server, 1); got[0] != "human" {
		t.Fatalf("assignee = %q, want concurrent human assignment preserved", got[0])
	}
	report := readAssignmentReport(t)
	if len(report.Assignments) != 0 || report.Unassigned != 0 || !report.NoWork {
		t.Fatalf("report = %+v, want skipped concurrent assignment excluded from mutations and remainder", report)
	}
}

func TestBacklogAssignmentInvalidRosterFailsBeforeMutation(t *testing.T) {
	root, server := assignmentCommandFixture(t)
	t.Setenv("GOOBERS_INPUT_STRATEGY", assignmentStrategyConstantCap)
	t.Setenv("GOOBERS_INPUT_ROSTER", `[]`)

	code, _, stderr := runArgs(t, "backlog-assignment", root)
	if code != 1 || !strings.Contains(stderr, "roster must contain at least one assignee") {
		t.Fatalf("code = %d, stderr = %q, want fail-closed roster error", code, stderr)
	}
	if got := fakeIssueAssignees(server, 1); got[0] != "" {
		t.Fatalf("item was assigned despite invalid roster: %v", got)
	}
	if requests := server.issueListRequestCount(); requests != 0 {
		t.Fatalf("provider received %d list requests before roster validation, want 0", requests)
	}
}

func assignmentCommandFixture(t *testing.T) (string, *fakeGitHubServer) {
	t.Helper()
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(1, "Ready item", "goobers:approved", "goobers:ready")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "assignment-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-assignment")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", "goobers/status:in-review")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "backlog-assignment.json")
	t.Chdir(t.TempDir())
	return root, server
}

func setFakeIssueAssignee(server *fakeGitHubServer, number int, assignee string) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.issues[number].assignee = assignee
}

func setFakeIssueLabels(server *fakeGitHubServer, number int, labels ...string) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.issues[number].labels = append([]string(nil), labels...)
}

func fakeIssueAssignees(server *fakeGitHubServer, numbers ...int) []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	assignees := make([]string, len(numbers))
	for i, number := range numbers {
		assignees[i] = server.issues[number].assignee
	}
	return assignees
}

func readAssignmentReport(t *testing.T) backlogAssignmentReport {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(".", "backlog-assignment.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report backlogAssignmentReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	return report
}
