package main

import (
	"bufio"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// ci.yml's header describes which jobs the merge gate requires. It advertised a
// `conformance` job for months after that job was deleted (#4224), because
// nothing checked the prose against the workflow. These tests make the header's
// two job lists machine-checked: the group list against test/ci's own group
// constants, and both lists together against required-ci's `needs:`.

// TestCIHeaderMatchesRequiredJobs fails when the header, the group constants,
// and required-ci's dependency list disagree.
func TestCIHeaderMatchesRequiredJobs(t *testing.T) {
	t.Parallel()

	groups, dedicated := parseCIHeaderJobs(t)

	wantGroups := slices.Sorted(maps.Keys(knownGroups))
	if !reflect.DeepEqual(groups, wantGroups) {
		t.Errorf("ci.yml header group jobs = %q, want test/ci's groups %q", groups, wantGroups)
	}

	announced := slices.Sorted(slices.Values(slices.Concat(groups, dedicated)))
	required := slices.Sorted(slices.Values(loadCIWorkflow(t).Jobs["required-ci"].Needs))
	if len(required) == 0 {
		t.Fatal("ci.yml's required-ci job declares no needs")
	}
	if !reflect.DeepEqual(announced, required) {
		t.Errorf("ci.yml header announces jobs %q, required-ci needs %q", announced, required)
	}
}

// TestCIHeaderNamesOnlyDefinedJobs catches the exact regression #4224 reported:
// a header naming a job the workflow no longer defines.
func TestCIHeaderNamesOnlyDefinedJobs(t *testing.T) {
	t.Parallel()

	groups, dedicated := parseCIHeaderJobs(t)
	defined := loadCIWorkflow(t).Jobs
	for _, job := range slices.Concat(groups, dedicated) {
		if _, ok := defined[job]; !ok {
			t.Errorf("ci.yml header names job %q, which the workflow does not define", job)
		}
	}
}

// TestContributingRequiredJobTableMatchesWorkflowNames keeps the contributor
// map tied to the display names users actually see in required-check output.
// The table covers every required-ci dependency plus the aggregate itself;
// neither invented names nor silently omitted gates are allowed.
func TestContributingRequiredJobTableMatchesWorkflowNames(t *testing.T) {
	t.Parallel()

	workflow := loadCIWorkflow(t)
	required := workflow.Jobs["required-ci"].Needs
	want := make([]string, 0, len(required)+1)
	for _, id := range required {
		job, ok := workflow.Jobs[id]
		if !ok {
			t.Fatalf("required-ci needs undefined job %q", id)
		}
		if strings.TrimSpace(job.Name) == "" {
			t.Fatalf("required job %q has no display name", id)
		}
		want = append(want, job.Name)
	}
	want = append(want, workflow.Jobs["required-ci"].Name)
	slices.Sort(want)

	data, err := os.ReadFile(filepath.Join(moduleRoot(t), "CONTRIBUTING.md"))
	if err != nil {
		t.Fatalf("read CONTRIBUTING.md: %v", err)
	}
	const start = "<!-- ci-required-jobs:start -->"
	const end = "<!-- ci-required-jobs:end -->"
	_, rest, ok := strings.Cut(string(data), start)
	if !ok {
		t.Fatalf("CONTRIBUTING.md omits %q", start)
	}
	section, _, ok := strings.Cut(rest, end)
	if !ok {
		t.Fatalf("CONTRIBUTING.md omits %q", end)
	}
	var got []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		name, _, ok := strings.Cut(strings.TrimPrefix(line, "| `"), "`")
		if !ok {
			t.Fatalf("malformed CI job table row %q", line)
		}
		if seen[name] {
			t.Errorf("CONTRIBUTING.md repeats CI job %q", name)
		}
		seen[name] = true
		got = append(got, name)
	}
	slices.Sort(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CONTRIBUTING.md CI jobs = %q, want workflow display names %q", got, want)
	}
}

// parseCIHeaderJobs reads the two job lists out of ci.yml's leading comment
// block: the indented `group jobs:` table, one job per line, and the
// comma-joined `dedicated jobs:` list, which may wrap onto continuation lines.
// A blank comment line ends either list, and the `on:` key ends the header.
func parseCIHeaderJobs(t *testing.T) (groups, dedicated []string) {
	t.Helper()

	file, err := os.Open(filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("open CI workflow: %v", err)
	}
	defer func() { _ = file.Close() }()

	const (
		outside = iota
		inGroups
		inDedicated
	)
	section := outside
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if strings.HasPrefix(line, "on:") {
			break
		}
		if !strings.HasPrefix(line, "#") {
			section = outside
			continue
		}
		trimmed := strings.TrimSpace(strings.TrimPrefix(line, "#"))

		switch {
		case trimmed == "group jobs:":
			section = inGroups
		case strings.HasPrefix(trimmed, "dedicated jobs:"):
			section = inDedicated
			dedicated = append(dedicated, splitCIJobList(strings.TrimPrefix(trimmed, "dedicated jobs:"))...)
		case trimmed == "":
			section = outside
		case section == inGroups:
			groups = append(groups, strings.Fields(trimmed)[0])
		case section == inDedicated:
			dedicated = append(dedicated, splitCIJobList(trimmed)...)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	if len(groups) == 0 {
		t.Fatal("ci.yml header has no `group jobs:` list")
	}
	if len(dedicated) == 0 {
		t.Fatal("ci.yml header has no `dedicated jobs:` list")
	}
	slices.Sort(groups)
	slices.Sort(dedicated)
	return groups, dedicated
}

func splitCIJobList(text string) []string {
	var jobs []string
	for _, field := range strings.Split(text, ",") {
		if name := strings.TrimSpace(field); name != "" {
			jobs = append(jobs, name)
		}
	}
	return jobs
}
