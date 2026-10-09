package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdGoobersGrowthGateUsesBaseRevision(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(data)
	job := workflowJob(workflow, "cmdgoobers-growth")
	for _, want := range []string{
		"fetch-depth: 0",
		"BASE_REF: ${{ github.event.pull_request.base.sha || github.event.merge_group.base_sha || inputs.base_sha || 'HEAD' }}",
		"HEAD_REF: ${{ github.event.pull_request.head.sha || github.event.merge_group.head_sha || inputs.head_sha || '' }}",
		`run: go run ./test/cmdgoobersgrowth -base-ref "$BASE_REF" -head-ref "$HEAD_REF"`,
	} {
		if !strings.Contains(job, want) {
			t.Errorf("growth gate missing %q", want)
		}
	}
	aggregate := workflowJob(workflow, "required-ci")
	for _, want := range []string{
		"CI_NEEDS: ${{ toJSON(needs) }}",
		"run: go run ./test/cipolicy gate",
		"needs: [scope, preflight, cmdgoobers-growth,",
	} {
		if !strings.Contains(aggregate, want) {
			t.Errorf("required-ci missing %q", want)
		}
	}
}
