package main

import (
	"strings"
	"testing"
)

func releasePublisherScript(t *testing.T) string {
	t.Helper()
	workflow := loadReleaseAuthorizationWorkflow(t)
	for _, step := range workflow.Jobs["verify-and-publish"].Steps {
		if step.Name == "Publish GitHub Release" {
			return step.Run
		}
	}
	t.Fatal("missing final publication step")
	return ""
}

func TestReleasePublisherNeverExecutesReleaseContent(t *testing.T) {
	workflow := loadReleaseAuthorizationWorkflow(t)
	publisher := workflow.Jobs["verify-and-publish"]
	// Keep an exact step allowlist: introducing another executable verification
	// step into this contents:write job must require deliberate policy review.
	expected := []string{"Checkout", "Authorize release source", "Download immutable final signed artifacts", "Download validated release notes as data", "Publish GitHub Release"}
	if len(publisher.Steps) != len(expected) {
		t.Fatalf("publisher acquired extra executable steps: %d", len(publisher.Steps))
	}
	for index, step := range publisher.Steps {
		if step.Name != expected[index] || step.If != "" || step.ContinueOnError {
			t.Fatalf("unconditional publisher step order changed: %s", step.Name)
		}
	}
	script := releasePublisherScript(t)
	for _, unsafe := range []string{"go run", "go build", "npm ", "sh dist/", "./dist/", "./smoke/", "tar -", "unzip ", "dist/*", "source ", "eval "} {
		if strings.Contains(script, unsafe) {
			t.Errorf("publisher executes or broadly uploads release content: %q", unsafe)
		}
	}
	for _, required := range []string{"set -euo pipefail", "python3 -I - <<'PYVERIFY'", "mapfile -t assets < publish-assets.txt", `gh release upload "$TAG" "${assets[@]}" --clobber`, `gh release create "$TAG" "${assets[@]}"`, "--notes-file release-notes/RELEASE_NOTES.md"} {
		if !strings.Contains(script, required) {
			t.Errorf("publisher lacks inert verification or exact upload contract: %q", required)
		}
	}
	validation := workflow.Jobs["validate-release"]
	if validation.Permissions["contents"] != "read" {
		t.Fatal("executable validation must be read-only")
	}
	for _, step := range validation.Steps {
		if step.Env["GH_TOKEN"] != "" {
			t.Fatalf("validation step received publication token: %s", step.Name)
		}
	}
}

func TestReleasePublisherUsesImmutableSignerAndNotesArtifactIDs(t *testing.T) {
	workflow := loadReleaseAuthorizationWorkflow(t)
	signer := workflow.Jobs["sign-windows"]
	if signer.Outputs["signed-artifact-id"] != "${{ steps.final-signed-artifacts.outputs.artifact-id }}" {
		t.Fatal("signer must expose its final replacement artifact identity")
	}
	found := false
	for _, step := range signer.Steps {
		if step.ID == "final-signed-artifacts" {
			found = step.With["overwrite"] == "true" && step.With["name"] == "dist-signed"
		}
	}
	if !found {
		t.Fatal("signed artifact identity must come from the final overwrite upload")
	}
	validation := workflow.Jobs["validate-release"]
	if validation.Outputs["notes-artifact-id"] != "${{ steps.release-notes-upload.outputs.artifact-id }}" {
		t.Fatal("validation must expose a separate notes artifact identity")
	}
	for _, jobName := range []string{"validate-release", "verify-and-publish"} {
		for _, step := range workflow.Jobs[jobName].Steps {
			switch step.With["path"] {
			case "dist":
				if step.With["artifact-ids"] != "${{ needs.sign-windows.outputs.signed-artifact-id }}" || step.With["name"] != "" || step.With["merge-multiple"] != "true" {
					t.Fatalf("%s must independently download immutable signer output", jobName)
				}
			case "release-notes":
				if step.With["artifact-ids"] != "${{ needs.validate-release.outputs.notes-artifact-id }}" || step.With["name"] != "" || step.With["merge-multiple"] != "true" {
					t.Fatal("publisher must download exact notes artifact")
				}
			}
			if step.ID == "release-notes-upload" && step.With["path"] != "dist/RELEASE_NOTES.md" {
				t.Fatal("validation may transfer only notes, never mutated release assets")
			}
		}
	}
}
