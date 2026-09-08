//go:build integration && !windows

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Execute the workflow's real retention script against local inert fixtures.
// Docker is a shell function: this never starts a container or reads a daemon.
func TestIntegrationReleaseImageEvidenceRetention(t *testing.T) {
	testdep.Require(t, "bash", "cp", "find", "mkdir")
	workflow := loadReleaseAuthorizationWorkflow(t)
	var script string
	for _, step := range workflow.Jobs["native-linux-images"].Steps {
		if step.Name == "Retain native image provenance and failure evidence" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("missing retention script")
	}
	for _, scenario := range []string{"complete", "missing proof", "empty proof", "missing provenance", "copy failure", "inspect failure", "already failed"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			writeReleaseImageEvidenceFixture(t, root)
			outcome := "success"
			proof := filepath.Join(root, "verified-image-contexts", "image-evidence.json")
			switch scenario {
			case "missing proof", "already failed":
				if err := os.Remove(proof); err != nil {
					t.Fatal(err)
				}
			case "empty proof":
				if err := os.Truncate(proof, 0); err != nil {
					t.Fatal(err)
				}
			case "missing provenance":
				if err := os.Remove(filepath.Join(root, "verified-image-contexts", "linux-arm64", "artifact-import.json")); err != nil {
					t.Fatal(err)
				}
			}
			stub := releaseImageEvidenceDockerStub
			if scenario == "copy failure" {
				stub += "\ncp() { return 37; }\n"
			}
			if scenario == "inspect failure" {
				stub += "\ndocker() { return 42; }\n"
			}
			if scenario == "already failed" {
				outcome = "failure"
			}
			cmd := exec.Command("bash", "--noprofile", "--norc", "-c", stub+script)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "IMAGE_PLATFORM=linux-arm64", "IMAGE_VERIFY_PREFIX=fixture", "IMAGE_VERIFICATION_OUTCOME="+outcome)
			output, err := cmd.CombinedOutput()
			wantSuccess := scenario == "complete" || scenario == "already failed"
			if (err == nil) != wantSuccess {
				t.Fatalf("retention success=%v, want %v: %v\n%s", err == nil, wantSuccess, err, output)
			}
			if scenario == "complete" {
				inspect, err := os.ReadFile(filepath.Join(root, "native-image-evidence", "image-inspect.json"))
				if err != nil {
					t.Fatal(err)
				}
				var images []map[string]string
				if err := json.Unmarshal(inspect, &images); err != nil || len(images) != 3 {
					t.Fatalf("inspection must be one valid array containing all three images: %s (%v)", inspect, err)
				}
			}
		})
	}
}

func writeReleaseImageEvidenceFixture(t *testing.T, root string) {
	t.Helper()
	files := []string{"native-image-evidence/release-source.json", "native-image-evidence/final-archive-SHA256SUMS", "native-image-evidence/release-image-verification.txt", "verified-image-contexts/image-evidence.json", "verified-image-contexts/linux-arm64/artifact-import.json"}
	for _, directory := range []string{"original-image-inputs/linux-arm64", "verified-image-contexts/linux-arm64"} {
		files = append(files, directory+"/SHA256SUMS", directory+"/release.json")
	}
	for _, harness := range []string{"copilot", "claude"} {
		directory := "verified-image-contexts/harness-" + harness
		files = append(files, directory+"/SHA256SUMS", directory+"/source", directory+"/version")
	}
	for _, file := range files {
		path := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

var releaseImageEvidenceDockerStub = strings.TrimSpace(`
docker() {
  if [[ "$1 $2" == "image ls" ]]; then
    printf '%s\n' sha256:a sha256:b sha256:c
  elif [[ "$1 $2" == "image inspect" ]]; then
    shift 2
    separator=''
    printf '['
    for id in "$@"; do
      printf '%s{"Id":"%s"}' "$separator" "$id"
      separator=','
    done
    printf ']\n'
  else
    return 64
  fi
}
`) + "\n"
