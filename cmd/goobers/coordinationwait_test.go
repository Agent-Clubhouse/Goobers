package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/providers"
)

func TestCoordinationWaitCannotBeDisabledByParkFilter(t *testing.T) {
	for _, filter := range []string{"true", "false"} {
		predicate, _, err := compileBacklogLabelSelection("", []string{providers.LabelApproved}, nil, "", filter)
		if err != nil {
			t.Fatal(err)
		}
		matched, err := predicate.Matches([]string{providers.LabelApproved, providers.LabelReady, providers.LabelCoordinationWait})
		if err != nil || matched {
			t.Fatalf("coordination wait bypassed: match=%v err=%v", matched, err)
		}
	}
}

func TestCoordinationWaitExcludedFromCurationAndResweeps(t *testing.T) {
	for _, mode := range []string{"forward", "resweep"} {
		t.Run(mode, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(1, "Waiting forward", providers.LabelApproved, providers.LabelCoordinationWait)
			server.addIssue(2, "Waiting ready sweep", providers.LabelApproved, providers.LabelReady, providers.LabelCoordinationWait)
			server.addIssue(3, "Waiting blocked sweep", providers.LabelApproved, blockedOnSiblingLabel, providers.LabelCoordinationWait)
			server.addIssue(4, "Ordinary admitted work", providers.LabelApproved)
			server.addIssue(5, "Ordinary review context", providers.LabelApproved, providers.LabelReady, inReviewStatusLabel)
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "wait-filter-run")
			configureCurationResweep(t, "4", "")
			t.Setenv("GOOBERS_INPUT_RESWEEPREADYLABEL", "")
			t.Setenv("GOOBERS_INPUT_RECONCILEMETADATA", "false")
			t.Setenv("GOOBERS_INPUT_FILTERPARKLABELS", "false")
			args := []string{"backlog-query", "--claim", "--debug"}
			if mode == "resweep" {
				t.Setenv("GOOBERS_INPUT_RESWEEPMAXITEMS", "4")
				args = append(args, "--resweep")
			}
			dir := t.TempDir()
			t.Chdir(dir)
			code, _, stderr := runArgs(t, append(args, root)...)
			if code != 0 {
				t.Fatalf("query=%d %s", code, stderr)
			}
			items := readCurationItems(t, filepath.Join(dir, "claimed-items.json"))
			want := "4"
			if mode == "resweep" {
				want = "5"
			}
			if len(items) != 1 || items[0].ID != want {
				t.Fatalf("waiting child admitted or ordinary admission changed: %+v", items)
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			for _, id := range []int{1, 2, 3} {
				if hasAnyLabel(server.issues[id].labels, []string{providers.LabelClaimed}) {
					t.Fatalf("waiting child %d was claimed", id)
				}
			}
		})
	}
}

func TestCoordinationWaitExcludedFromAssignmentAndLateAssignment(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "listed", true: "after-selection"}[late], func(t *testing.T) {
			root, server := assignmentCommandFixture(t)
			t.Setenv("GOOBERS_INPUT_STRATEGY", assignmentStrategyRoundRobin)
			t.Setenv("GOOBERS_INPUT_ROSTER", `[{"assignee":"alice"}]`)
			park := func() {
				setFakeIssueLabels(server, 1, providers.LabelApproved, providers.LabelReady, providers.LabelCoordinationWait)
			}
			if !late {
				park()
			}
			var stdout, stderr bytes.Buffer
			code := runBacklogAssignmentWithMutationHook([]string{root}, &stdout, &stderr, func(assignmentPlanEntry) { park() })
			if code != 0 {
				t.Fatalf("assignment=%d %s", code, stderr.String())
			}
			if strings.Join(fakeIssueAssignees(server, 1), "") != "" {
				t.Fatal("waiting child assigned")
			}
		})
	}
}
