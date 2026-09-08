package main

import (
	"maps"
	"regexp"
	"strings"
	"testing"
)

func TestReleaseBuildExportsOriginalImageInputsAndExactStamps(t *testing.T) {
	workflow := loadReleaseAuthorizationWorkflow(t)
	build := workflow.Jobs["build"]
	wantOutputs := map[string]string{
		"source-commit": "${{ steps.release-source.outputs.commit }}",
		"version":       "${{ steps.archive-build.outputs.version }}",
		"commit":        "${{ steps.archive-build.outputs.commit }}",
		"date":          "${{ steps.archive-build.outputs.date }}",
	}
	if !maps.Equal(build.Outputs, wantOutputs) {
		t.Fatalf("image verification must receive the original authorized build identities: %v", build.Outputs)
	}
	var archiveRun string
	originalUpload := false
	for _, step := range build.Steps {
		if step.ID == "archive-build" {
			archiveRun = step.Run
		}
		if step.With["name"] == "original-image-inputs" {
			originalUpload = step.With["path"] == "original-image-inputs/" && step.With["include-hidden-files"] == "true" && step.With["if-no-files-found"] == "error"
		}
	}
	if !originalUpload {
		t.Fatal("original image contexts, including checksummed .dockerignore, must survive artifact transfer")
	}
	for _, required := range []string{
		"-output dist", "-image-contexts original-image-inputs", "-image-targets linux/amd64,linux/arm64,windows/amd64",
		`echo "version=$TAG" >> "$GITHUB_OUTPUT"`, `echo "commit=$commit" >> "$GITHUB_OUTPUT"`, `echo "date=$date" >> "$GITHUB_OUTPUT"`,
	} {
		if !strings.Contains(archiveRun, required) {
			t.Errorf("archive build omits original image input or exact stamp export %q", required)
		}
	}
	if regexp.MustCompile(`(^|\s)-targets(?:\s|=)`).MatchString(archiveRun) {
		t.Fatal("image preparation must not narrow the default five-platform archive matrix")
	}
}

func TestReleaseNativeImageJobsImportSignedArtifactsWithoutRebuild(t *testing.T) {
	workflow := loadReleaseAuthorizationWorkflow(t)
	for _, name := range []string{"native-linux-images", "native-windows-image"} {
		t.Run(name, func(t *testing.T) {
			job := workflow.Jobs[name]
			if job.ContinueOnError {
				t.Fatal("native image failures must block release publication")
			}
			for _, key := range []string{"version", "commit", "date"} {
				if job.Env["RELEASE_"+strings.ToUpper(key)] != "${{ needs.build.outputs."+key+" }}" {
					t.Errorf("%s must consume the archive's exact embedded stamp", key)
				}
			}
			var commands strings.Builder
			var importRun string
			downloads := make(map[string]string)
			retains, uploads, checkout := false, false, false
			for _, step := range job.Steps {
				commands.WriteString(step.Run)
				key := step.With["name"]
				if step.With["artifact-ids"] == "${{ needs.sign-windows.outputs.signed-artifact-id }}" && step.With["merge-multiple"] == "true" {
					key = "dist-signed"
				}
				downloads[key] = step.With["path"]
				if step.Name == "Checkout authorized image tooling" {
					checkout = step.With["ref"] == "${{ needs.build.outputs.source-commit }}" && step.With["persist-credentials"] == "false"
				}
				if strings.Contains(step.Run, "go run ./release") {
					importRun = step.Run
					if step.If != "" || step.ContinueOnError {
						t.Fatal("native verification must be unconditional and failures must propagate")
					}
				}
				if step.Name == "Retain native image provenance and failure evidence" {
					retains = step.If == "always()" && strings.Contains(step.Run, "original-image-inputs") && strings.Contains(step.Run, "verified-image-contexts") && strings.Contains(step.Run, "image-inspect.json")
				}
				if step.Name == "Upload native image verification evidence" {
					uploads = step.If == "always()" && step.With["path"] == "native-image-evidence/"
				}
			}
			if !checkout || downloads["dist-signed"] != "dist" || downloads["original-image-inputs"] != "original-image-inputs" || !retains || !uploads {
				t.Fatal("native job must preserve source identity, final signed archives, original operator inputs and failure evidence")
			}
			for _, required := range []string{"-image-artifacts dist", "-image-inputs original-image-inputs", "-image-contexts verified-image-contexts", "-build-images -image-prefix", "-version", "-commit", "-date", "-targets", "image-evidence.json", "release-image-verification.txt", "release-source.json"} {
				if !strings.Contains(importRun, required) {
					t.Errorf("native import invocation omits %q", required)
				}
			}
			for _, forbidden := range []string{"go build", "npm ", "docker build", "docker push", "docker login", "-first-feature-snapshot", "-image-targets", "-output "} {
				if strings.Contains(commands.String(), forbidden) {
					t.Errorf("native artifact verification must not rebuild or publish: %q", forbidden)
				}
			}
		})
	}
}

func TestReleaseImagesUseNativeRunnerMatrix(t *testing.T) {
	workflow := loadReleaseAuthorizationWorkflow(t)
	linux := workflow.Jobs["native-linux-images"]
	got := make(map[string]string)
	for _, row := range linux.Strategy.Matrix.Include {
		got[row.Target] = row.Runner
	}
	want := map[string]string{"linux/amd64": "ubuntu-24.04", "linux/arm64": "ubuntu-24.04-arm"}
	if !maps.Equal(got, want) || len(linux.Strategy.Matrix.Include) != 2 || linux.RunsOn != "${{ matrix.runner }}" {
		t.Fatalf("Linux image verification must run natively on both architectures: %v", got)
	}
	windows := workflow.Jobs["native-windows-image"]
	if windows.RunsOn != "windows-2022" || windows.Env["DOCKER_BUILDKIT"] != "0" {
		t.Fatal("Windows image verification requires the Server 2022 native container engine")
	}
}

func TestReleaseSuccessfulImagesRequireRetainedEvidence(t *testing.T) {
	workflow := loadReleaseAuthorizationWorkflow(t)
	for _, name := range []string{"native-linux-images", "native-windows-image"} {
		job := workflow.Jobs[name]
		verificationID := ""
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "go run ./release") {
				verificationID = step.ID
			}
			if step.Name == "Retain native image provenance and failure evidence" {
				if step.Env["IMAGE_VERIFICATION_OUTCOME"] != "${{ steps.image-verification.outcome }}" || step.ContinueOnError {
					t.Errorf("%s retention must fail closed after successful verification", name)
				}
				for _, required := range []string{"success", "Required native image evidence missing", "verified-image-contexts/image-evidence.json", "artifact-import.json"} {
					if !strings.Contains(step.Run, required) {
						t.Errorf("%s omits mandatory evidence guard %q", name, required)
					}
				}
			}
			if step.Name == "Upload native image verification evidence" && (step.With["if-no-files-found"] != "error" || step.ContinueOnError) {
				t.Errorf("%s must fail if evidence cannot be uploaded", name)
			}
		}
		if verificationID != "image-verification" {
			t.Errorf("%s must expose native verification outcome to evidence retention", name)
		}
	}
}
