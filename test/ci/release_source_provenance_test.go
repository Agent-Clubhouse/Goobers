package main

import (
	"strings"
	"testing"
)

func TestReleasePublisherRequiresCleanFullSourceAndExternalStaging(t *testing.T) {
	workflow := loadReleaseAuthorizationWorkflow(t)
	var baseline, build, upload bool
	for _, step := range workflow.Jobs["build"].Steps {
		switch step.Name {
		case "Resolve feature baseline":
			baseline = true
			for _, marker := range []string{`set -euo pipefail`, `mktemp -d "$RUNNER_TEMP/goobers-release-baseline.XXXXXX"`,
				`--dir "$baseline_dir"`, `path=$baseline_dir/feature-registry.json`, `support-path=$baseline_dir/dsl-support-matrix.json`} {
				if !strings.Contains(step.Run, marker) {
					t.Errorf("baseline staging missing %q", marker)
				}
			}
			if strings.Contains(step.Run, ".release-baseline/") {
				t.Error("baseline dirties the source checkout")
			}
		case "Build release artifacts":
			build = true
			if step.If != "" || step.ContinueOnError ||
				step.Env["COMMIT_SHA"] != "${{ steps.release-source.outputs.commit }}" ||
				step.Env["IMAGE_CONTEXTS"] != "${{ runner.temp }}/original-image-inputs" {
				t.Fatal("publisher source check must be unconditional and use authorized source/external image staging")
			}
			for _, marker := range []string{`set -euo pipefail`, `-source-commit "$COMMIT_SHA"`, `-image-contexts "$IMAGE_CONTEXTS"`} {
				if !strings.Contains(step.Run, marker) {
					t.Errorf("publisher missing %q", marker)
				}
			}
		case "Upload original image inputs":
			upload = true
			if step.With["name"] != "original-image-inputs" || step.With["path"] != "${{ runner.temp }}/original-image-inputs/" {
				t.Fatal("upload must retain the external context under its existing artifact contract")
			}
		}
	}
	if !baseline || !build || !upload {
		t.Fatal("missing source provenance pipeline step")
	}
}
