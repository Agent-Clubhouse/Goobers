package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestLifecycleLabelContractsRejectDrift(t *testing.T) {
	tests := []struct {
		name     string
		workflow string
		want     string
	}{
		{
			name: "implementation trust label replacement",
			workflow: lifecycleWorkflow("implementation", `    - name: query-backlog
      type: deterministic
      goal: Claim one ready item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approve"
        requireLabels: "goobers:ready"
        excludeLabels: "goobers/status:in-review"
`),
			want: `task "query-backlog" input "trustLabel" configured lifecycle label "goobers:approve"; expected "goobers:approved"`,
		},
		{
			name: "implementation ready require label typo",
			workflow: lifecycleWorkflow("implementation", `    - name: query-backlog
      type: deterministic
      goal: Claim one ready item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        requireLabels: "goobers:ready2"
        excludeLabels: "goobers/status:in-review"
`),
			want: `task "query-backlog" input "requireLabels" configured lifecycle label "goobers:ready2"; expected "goobers:ready"`,
		},
		{
			name: "implementation in-review exclusion typo",
			workflow: lifecycleWorkflow("implementation", `    - name: query-backlog
      type: deterministic
      goal: Claim one ready item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        requireLabels: "goobers:ready"
        excludeLabels: "goobers/status:in-review2"
`),
			want: `task "query-backlog" input "excludeLabels" configured lifecycle label "goobers/status:in-review2"; expected "goobers/status:in-review"`,
		},
		{
			name: "implementation ready require label replacement",
			workflow: lifecycleWorkflow("implementation", `    - name: query-backlog
      type: deterministic
      goal: Claim one ready item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        requireLabels: "goobers:read"
        excludeLabels: "goobers/status:in-review"
`),
			want: `task "query-backlog" input "requireLabels" configured lifecycle label "goobers:read"; expected "goobers:ready"`,
		},
		{
			name: "ready health label typo",
			workflow: lifecycleWorkflow("backlog-curation", `    - name: sample-ready-pool
      type: deterministic
      goal: Snapshot ready items.
      run:
        command: ["goobers", "backlog-health"]
      inputs:
        trustLabel: "goobers:approved"
        readyLabel: "goobers:ready2"
`),
			want: `task "sample-ready-pool" input "readyLabel" configured lifecycle label "goobers:ready2"; expected "goobers:ready"`,
		},
		{
			name: "curation trust label replacement",
			workflow: lifecycleWorkflow("backlog-curation", `    - name: query-backlog
      type: deterministic
      goal: Claim curation work.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        curation: "true"
        trustLabel: "goobers:approve"
        excludeLabels: "goobers:ready"
        parkLabels: "goobers:needs-human,goobers:blocked-on-sibling,goobers:needs-remediation"
`),
			want: `task "query-backlog" input "trustLabel" configured lifecycle label "goobers:approve"; expected "goobers:approved"`,
		},
		{
			name: "curation ready exclusion typo",
			workflow: lifecycleWorkflow("backlog-curation", `    - name: query-backlog
      type: deterministic
      goal: Claim curation work.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        curation: "true"
        trustLabel: "goobers:approved"
        excludeLabels: "goobers:ready2"
        parkLabels: "goobers:needs-human,goobers:blocked-on-sibling,goobers:needs-remediation"
`),
			want: `task "query-backlog" input "excludeLabels" configured lifecycle label "goobers:ready2"; expected "goobers:ready"`,
		},
		{
			name: "curation needs-human park label typo",
			workflow: lifecycleWorkflow("backlog-curation", `    - name: query-backlog
      type: deterministic
      goal: Claim curation work.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        curation: "true"
        trustLabel: "goobers:approved"
        excludeLabels: "goobers:ready"
        parkLabels: "goobers:needs-human2,goobers:blocked-on-sibling,goobers:needs-remediation"
`),
			want: `task "query-backlog" input "parkLabels" configured lifecycle label "goobers:needs-human2"; expected "goobers:needs-human"`,
		},
		{
			name: "curation blocked-on-sibling park label typo",
			workflow: lifecycleWorkflow("backlog-curation", `    - name: query-backlog
      type: deterministic
      goal: Claim curation work.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        curation: "true"
        trustLabel: "goobers:approved"
        excludeLabels: "goobers:ready"
        parkLabels: "goobers:needs-human,goobers:blocked-on-sibling2,goobers:needs-remediation"
`),
			want: `task "query-backlog" input "parkLabels" configured lifecycle label "goobers:blocked-on-sibling2"; expected "goobers:blocked-on-sibling"`,
		},
		{
			name: "curation needs-remediation park label typo",
			workflow: lifecycleWorkflow("backlog-curation", `    - name: query-backlog
      type: deterministic
      goal: Claim curation work.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        curation: "true"
        trustLabel: "goobers:approved"
        excludeLabels: "goobers:ready"
        parkLabels: "goobers:needs-human,goobers:blocked-on-sibling,goobers:needs-remediation2"
`),
			want: `task "query-backlog" input "parkLabels" configured lifecycle label "goobers:needs-remediation2"; expected "goobers:needs-remediation"`,
		},
		{
			name: "resweep ready label typo",
			workflow: lifecycleWorkflow("curate-resweep", `    - name: query-resweep
      type: deterministic
      goal: Re-sweep parked and ready items.
      run:
        command: ["goobers", "backlog-query", "--claim", "--resweep"]
      inputs:
        trustLabel: "goobers:approved"
        excludeLabels: "goobers:ready"
        parkLabels: "goobers:needs-human,goobers:blocked-on-sibling,goobers:needs-remediation"
        resweepReadyLabel: "goobers:ready2"
`),
			want: `task "query-resweep" input "resweepReadyLabel" configured lifecycle label "goobers:ready2"; expected "goobers:ready"`,
		},
		{
			name: "resweep trust label replacement",
			workflow: lifecycleWorkflow("curate-resweep", `    - name: query-resweep
      type: deterministic
      goal: Re-sweep parked and ready items.
      run:
        command: ["goobers", "backlog-query", "--claim", "--resweep"]
      inputs:
        trustLabel: "goobers:approve"
        excludeLabels: "goobers:ready"
        parkLabels: "goobers:needs-human,goobers:blocked-on-sibling,goobers:needs-remediation"
        resweepReadyLabel: "goobers:ready"
`),
			want: `task "query-resweep" input "trustLabel" configured lifecycle label "goobers:approve"; expected "goobers:approved"`,
		},
		{
			name: "recovery remediation require label typo",
			workflow: lifecycleWorkflow("implementation-recovery", `    - name: query-backlog
      type: deterministic
      goal: Claim one remediation item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        requireLabels: "goobers:needs-remediation2"
        filterParkLabels: "false"
        excludeLabels: "goobers:needs-human,goobers:blocked-on-sibling,goobers/status:in-review"
`),
			want: `task "query-backlog" input "requireLabels" configured lifecycle label "goobers:needs-remediation2"; expected "goobers:needs-remediation"`,
		},
		{
			name: "recovery trust label replacement",
			workflow: lifecycleWorkflow("implementation-recovery", `    - name: query-backlog
      type: deterministic
      goal: Claim one remediation item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approve"
        requireLabels: "goobers:needs-remediation"
        filterParkLabels: "false"
        excludeLabels: "goobers:needs-human,goobers:blocked-on-sibling,goobers/status:in-review"
`),
			want: `task "query-backlog" input "trustLabel" configured lifecycle label "goobers:approve"; expected "goobers:approved"`,
		},
		{
			name: "recovery needs-human exclusion typo",
			workflow: lifecycleWorkflow("implementation-recovery", `    - name: query-backlog
      type: deterministic
      goal: Claim one remediation item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        requireLabels: "goobers:needs-remediation"
        filterParkLabels: "false"
        excludeLabels: "goobers:needs-human2,goobers:blocked-on-sibling,goobers/status:in-review"
`),
			want: `task "query-backlog" input "excludeLabels" configured lifecycle label "goobers:needs-human2"; expected "goobers:needs-human"`,
		},
		{
			name: "recovery remediation require label replacement",
			workflow: lifecycleWorkflow("implementation-recovery", `    - name: query-backlog
      type: deterministic
      goal: Claim one remediation item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        requireLabels: "goobers:need-remediation"
        filterParkLabels: "false"
        excludeLabels: "goobers:needs-human,goobers:blocked-on-sibling,goobers/status:in-review"
`),
			want: `task "query-backlog" input "requireLabels" configured lifecycle label "goobers:need-remediation"; expected "goobers:needs-remediation"`,
		},
		{
			name: "recovery blocked-on-sibling exclusion typo",
			workflow: lifecycleWorkflow("implementation-recovery", `    - name: query-backlog
      type: deterministic
      goal: Claim one remediation item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        requireLabels: "goobers:needs-remediation"
        filterParkLabels: "false"
        excludeLabels: "goobers:needs-human,goobers:blocked-on-sibling2,goobers/status:in-review"
`),
			want: `task "query-backlog" input "excludeLabels" configured lifecycle label "goobers:blocked-on-sibling2"; expected "goobers:blocked-on-sibling"`,
		},
		{
			name: "recovery in-review exclusion typo",
			workflow: lifecycleWorkflow("implementation-recovery", `    - name: query-backlog
      type: deterministic
      goal: Claim one remediation item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        requireLabels: "goobers:needs-remediation"
        filterParkLabels: "false"
        excludeLabels: "goobers:needs-human,goobers:blocked-on-sibling,goobers/status:in-review2"
`),
			want: `task "query-backlog" input "excludeLabels" configured lifecycle label "goobers/status:in-review2"; expected "goobers/status:in-review"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/dsl3", func(t *testing.T) {
			report := validateLifecycleConfig(t, withLifecycleDSL(tt.workflow, "3.0"))
			requireLifecycleIssue(t, report, errorLifecycleLabelContract, Error, tt.want)
		})
		t.Run(tt.name+"/dsl2", func(t *testing.T) {
			requireNoLifecycleIssues(t, validateLifecycleConfig(t, tt.workflow))
		})
	}
}

// TestLifecycleLabelContractsDSL2SampleKeepsLoading pins the goobers-ms sample
// shape that broke on upgrade: a DSL 2.0 workflow named "implementation" that
// claims on trustLabel alone, with no goobers:ready gate anywhere.
func TestLifecycleLabelContractsDSL2SampleKeepsLoading(t *testing.T) {
	workflow := lifecycleWorkflow("implementation", `    - name: query-backlog
      type: deterministic
      goal: Claim one trust-approved item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        excludeLabels: "goobers/status:in-review,goobers:needs-human"
        resultFile: "claimed-item.json"
      capabilities:
        - github:issues:write
      policyActions:
        - claim-backlog-items
`)
	gaggle := "  backlog:\n    provider: github\n    project: acme/app\n    labels: [\"goobers:approved\"]\n"
	report := validateLifecycleConfigWithGaggle(t, gaggle, workflow)
	if report.HasErrors() {
		t.Fatalf("existing DSL 2.0 workflow must keep loading:\n%s", joinIssues(report))
	}
	requireNoLifecycleIssues(t, report)

	report = validateLifecycleConfigWithGaggle(t, gaggle, withLifecycleDSL(workflow, "3.0"))
	requireLifecycleIssue(t, report, errorLifecycleLabelContract, Error,
		`input "requireLabels" configured lifecycle label "goobers:approved"; expected "goobers:ready"`)

	report = validateLifecycleConfig(t, withLifecycleDSL(workflow, "3.0"))
	requireLifecycleIssue(t, report, errorLifecycleLabelContract, Error,
		`input "requireLabels" configured lifecycle label ""; expected "goobers:ready"`)
}

func TestLifecycleLabelContractsHonorGaggleRequireLabelsDefault(t *testing.T) {
	workflow := withLifecycleDSL(lifecycleWorkflow("implementation", `    - name: query-backlog
      type: deterministic
      goal: Claim one ready item.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers:approved"
        excludeLabels: "goobers/status:in-review"
`), "3.0")
	report := validateLifecycleConfigWithGaggle(t, "  backlog:\n    provider: github\n    project: acme/app\n  requireLabels: [\"goobers:ready\"]\n", workflow)
	requireNoLifecycleIssues(t, report)
}

// TestLifecycleLabelContractsHoldForShippedWorkflows keeps #6153's guard on
// the workflows this repo ships. They are pinned to DSL 2.0, where LCL001 does
// not run for users, so the contract is applied here directly regardless of
// pin.
func TestLifecycleLabelContractsHoldForShippedWorkflows(t *testing.T) {
	roots := []string{"config-examples", "reference-workflows", "internal/instance", "templates", "examples"}
	checked := 0
	for _, root := range roots {
		err := filepath.WalkDir(filepath.Join("..", "..", root), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, doc := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n---") {
				if !strings.Contains(doc, "kind: Workflow") {
					continue
				}
				var w apiv1.Workflow
				if err := yaml.Unmarshal([]byte(doc), &w); err != nil || w.Kind != "Workflow" {
					continue
				}
				r := &Report{}
				checkLifecycleLabelContracts(r, w, path, nil)
				for _, issue := range r.Issues {
					t.Errorf("%s: %s", path, issue.Message)
				}
				checked++
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if checked == 0 {
		t.Fatal("found no shipped workflows to check")
	}
}

func requireNoLifecycleIssues(t *testing.T, report *Report) {
	t.Helper()
	for _, issue := range report.Issues {
		if issue.Code == errorLifecycleLabelContract {
			t.Fatalf("unexpected lifecycle label finding: %+v", issue)
		}
	}
}

func requireLifecycleIssue(t *testing.T, report *Report, code WarningCode, severity Severity, want string) {
	t.Helper()
	for _, issue := range report.Issues {
		if issue.Code == code && issue.Severity == severity && strings.Contains(issue.Message, want) {
			return
		}
	}
	t.Fatalf("want %s %s containing %q; issues =\n%s", severity, code, want, joinIssues(report))
}

func TestLifecycleLabelContractsAllowArbitrarySelectorLabels(t *testing.T) {
	tests := []struct {
		name     string
		workflow string
	}{
		{
			name: "arbitrary labels",
			workflow: lifecycleWorkflow("custom-selector", `    - name: query-backlog
      type: deterministic
      goal: Claim arbitrary partitioned work.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers"
        requireLabels: "goobers:cloud,area:backend"
        excludeLabels: "team:other"
        resultFile: "claimed-item.json"
      capabilities:
        - github:issues:write
      policyActions:
        - claim-backlog-items
`),
		},
		{
			name: "custom selectors sharing reserved prefixes",
			workflow: lifecycleWorkflow("custom-prefixed-selector", `    - name: query-backlog
      type: deterministic
      goal: Claim custom work with labels near lifecycle namespaces.
      run:
        command: ["goobers", "backlog-query", "--claim"]
      inputs:
        trustLabel: "goobers"
        requireLabels: "goobers:ready-for-docs,area:backend"
        excludeLabels: "goobers:needs-human-review,goobers/status:in-review-docs"
        parkLabels: "goobers:needs-remediation-followup,goobers:blocked-on-sibling-review"
        resultFile: "claimed-item.json"
      capabilities:
        - github:issues:write
      policyActions:
        - claim-backlog-items
`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := validateLifecycleConfig(t, tt.workflow)
			if report.HasErrors() {
				t.Fatalf("arbitrary non-lifecycle labels must remain supported:\n%s", joinIssues(report))
			}
		})
	}
}

func TestLifecycleLabelContractsAllowReferenceWorkflows(t *testing.T) {
	report, err := newV(t).ValidateDir("../../reference-workflows")
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	if report.HasErrors() {
		t.Fatalf("reference workflows must satisfy lifecycle label contracts:\n%s", joinIssues(report))
	}
}

func validateLifecycleConfig(t *testing.T, workflow string) *Report {
	t.Helper()
	return validateLifecycleConfigWithGaggle(t, "  backlog:\n    provider: github\n    project: acme/app\n", workflow)
}

func validateLifecycleConfigWithGaggle(t *testing.T, gaggleBacklog, workflow string) *Report {
	t.Helper()
	dir := t.TempDir()
	config := `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: local
spec:
  instance:
    name: local
    environment: dev
  gaggles: [acme]
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: acme
spec:
  project:
    provider: github
    owner: acme
    name: app
` + gaggleBacklog + `  isolation:
    namespace: gaggle-acme
---
` + workflow
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := newV(t).ValidateDir(dir)
	if err != nil {
		t.Fatalf("ValidateDir: %v", err)
	}
	return report
}

func lifecycleWorkflow(name, tasks string) string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: ` + name + `
spec:
  gaggle: acme
  triggers:
    - type: schedule
      schedule: "0 * * * *"
  start: ` + lifecycleStartTask(tasks) + `
  tasks:
` + tasks
}

// withLifecycleDSL repins a lifecycleWorkflow; preview 3.x pins carry their
// own per-workflow acknowledgement so DVL011 does not mask the finding.
func withLifecycleDSL(workflow, version string) string {
	workflow = strings.Replace(workflow, `dslVersion: "2.0"`, `dslVersion: "`+version+`"`, 1)
	return strings.Replace(workflow, "metadata:\n  name:",
		"metadata:\n  annotations:\n    goobers.dev/allow-preview-features: \"true\"\n  name:", 1)
}

func lifecycleStartTask(tasks string) string {
	for _, line := range strings.Split(tasks, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- name: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "- name: "))
		}
	}
	return "query-backlog"
}
