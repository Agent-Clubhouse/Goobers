package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		t.Run(tt.name, func(t *testing.T) {
			report := validateLifecycleConfig(t, tt.workflow)
			if !report.HasErrors() {
				t.Fatalf("expected lifecycle label drift to fail validation")
			}
			issues := joinIssues(report)
			if !strings.Contains(issues, tt.want) {
				t.Fatalf("issues =\n%s\nwant substring %q", issues, tt.want)
			}
		})
	}
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
  backlog:
    provider: github
    project: acme/app
  isolation:
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

func lifecycleStartTask(tasks string) string {
	for _, line := range strings.Split(tasks, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- name: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "- name: "))
		}
	}
	return "query-backlog"
}
