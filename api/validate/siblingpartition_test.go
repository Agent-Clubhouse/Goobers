package validate

import (
	"strings"
	"testing"
)

// siblingPartitionConfig is a gaggle partitioned on goobers:cloud whose
// declared sibling (same repo) claims goobers:laptop, plus one workflow with
// a single backlog-query task carrying inputsYAML.
func siblingPartitionConfig(inputsYAML string) string {
	return `apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: sibling-partition
spec:
  instance:
    name: sibling-partition
    environment: dev
  gaggles:
    - web
---
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: web
spec:
  project: {provider: github, owner: example, name: web}
  backlog:
    provider: github
    project: example/web
  isolation:
    namespace: web
  requireLabels:
    - goobers:cloud
  siblings:
    - project: {provider: github, owner: example, name: web}
      label: Laptop instance
      requireLabels:
        - goobers:laptop
---
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: implementation
spec:
  gaggle: web
  triggers:
    - type: manual
  start: query-backlog
  tasks:
    - name: query-backlog
      type: deterministic
      goal: claim work
      run:
        command: ["goobers", "backlog-query"]
      capabilities:
        - github:issues:write
` + inputsYAML
}

func siblingWarnings(report *Report) []string {
	var out []string
	for _, issue := range report.Issues {
		if issue.Code == WarningSiblingLabelOverlap {
			out = append(out, issue.Message)
		}
	}
	return out
}

// TestSiblingPartitionProbes pins #3286's two probes: a stage-level
// requireLabels override that drops the gaggle's partition label, and a
// require set that is disjoint from the sibling's without excluding it.
// Both previously validated clean.
func TestSiblingPartitionProbes(t *testing.T) {
	tests := []struct {
		name   string
		inputs string
		want   []string // substrings that must appear across SIB001 warnings
		clean  bool     // no SIB001 at all
	}{
		{
			name: "override drops the partition label",
			inputs: `      inputs:
        requireLabels: "goobers:ready"
`,
			want: []string{
				`task "query-backlog" overrides requireLabels to [goobers:ready]`,
				"drops partition label(s) [goobers:cloud]",
				`add excludeLabels: "goobers:laptop"`,
			},
		},
		{
			name:   "inherited partition is disjoint but not exclusive",
			inputs: "",
			want: []string{
				`is disjoint from declared sibling "Laptop instance"`,
				"an item carrying [goobers:cloud goobers:laptop] is claimable by both instances",
				`add excludeLabels: "goobers:laptop"`,
				"have the sibling exclude [goobers:cloud]",
			},
		},
		{
			name: "predicate requiring another label still contends",
			inputs: `      inputs:
        labelPredicate: '"goobers:ready" in labels'
`,
			want: []string{
				"an item carrying [goobers:cloud goobers:laptop goobers:ready] is claimable by both instances",
				`add excludeLabels: "goobers:laptop"`,
			},
		},
		{
			name: "mutual exclude partitions the backlog",
			inputs: `      inputs:
        excludeLabels: "goobers:laptop"
`,
			clean: true,
		},
		{
			name: "labelPredicate exclusion partitions the backlog",
			inputs: `      inputs:
        labelPredicate: '!("goobers:laptop" in labels)'
`,
			clean: true,
		},
		{
			name: "override that keeps the partition label and excludes",
			inputs: `      inputs:
        requireLabels: "goobers:ready,goobers:cloud"
        excludeLabels: "goobers:laptop"
`,
			clean: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := writeAndValidate(t, siblingPartitionConfig(tc.inputs))
			got := strings.Join(siblingWarnings(report), "\n")
			if tc.clean {
				if got != "" {
					t.Fatalf("want no SIB001, got:\n%s", got)
				}
				return
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("SIB001 warnings missing %q:\n%s", want, got)
				}
			}
		})
	}
}

// A sibling working a different repo can never contend, so a dropped
// partition label is not reported against it.
func TestSiblingPartitionIgnoresCrossRepoSibling(t *testing.T) {
	config := strings.Replace(siblingPartitionConfig(`      inputs:
        requireLabels: "goobers:ready"
`), "    - project: {provider: github, owner: example, name: web}", "    - project: {provider: github, owner: example, name: other}", 1)
	report := writeAndValidate(t, config)
	if got := siblingWarnings(report); len(got) != 0 {
		t.Fatalf("want no SIB001 for a cross-repo sibling, got:\n%s", strings.Join(got, "\n"))
	}
}
