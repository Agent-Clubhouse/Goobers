package shippedworkflows

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
)

// TestQualitySpotCheckSelectsLensesFromChangedPaths runs the reference
// selective parallel's real triage script against a fixture history and
// checks that it publishes a selection the shipped envelope accepts, naming
// exactly the lenses the changed paths call for.
func TestQualitySpotCheckSelectsLensesFromChangedPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the triage script is POSIX shell and pins runsOn.os: linux")
	}
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), "reference-workflows", "gaggles", "goobers", "workflows", "quality-spot-check.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var definition apiv1.Workflow
	if err := yaml.Unmarshal(raw, &definition); err != nil {
		t.Fatal(err)
	}
	triage := definition.Spec.Tasks[0]
	if triage.Name != "select-lenses" || triage.Run == nil || triage.Run.Script == "" {
		t.Fatalf("first task = %+v, want the select-lenses script", triage)
	}
	optional := map[string]bool{}
	for _, branch := range definition.Spec.Parallels[0].Branches {
		optional[branch.Name] = branch.Optional
	}
	schemas, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	// The script may rely on sh builtins and git only: pin PATH to exactly
	// those two so a new tool dependency fails here rather than on a runner
	// image that lacks it.
	tools := t.TempDir()
	shell := ""
	for _, name := range []string{"git", "sh"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("locate %s: %v", name, err)
		}
		if name == "sh" {
			shell = path
		}
		if err := os.Symlink(path, filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}

	for name, tc := range map[string]struct {
		files []string
		want  []string
	}{
		"security and tests": {files: []string{"internal/auth/token.go", "internal/auth/token_test.go"}, want: []string{"security", "testCoverage"}},
		"dependencies":       {files: []string{"go.sum", "portal/package-lock.json"}, want: []string{"dependencies"}},
		"nothing applies":    {files: []string{"docs/readme.md"}, want: []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			runGit(t, repo, "init", "--initial-branch=main")
			for _, file := range tc.files {
				path := filepath.Join(repo, filepath.FromSlash(file))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(file+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			runGit(t, repo, "add", "-A")
			runGit(t, repo, "-c", "user.email=contract@example.test", "-c", "user.name=contract", "commit", "-m", "change")

			command := exec.Command(shell, "-c", triage.Run.Script)
			command.Dir = repo
			command.Env = append(os.Environ(), "PATH="+tools,
				"GOOBERS_INPUT_SINCE="+triage.Inputs["since"],
				"GOOBERS_INPUT_ARTIFACTMANIFESTFILE="+triage.Inputs["artifactManifestFile"])
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("triage script: %v\n%s", err, output)
			}

			var manifest struct {
				Entries []struct{ Name, Path, MediaType string } `json:"entries"`
			}
			readJSON(t, filepath.Join(repo, triage.Inputs["artifactManifestFile"]), &manifest)
			if len(manifest.Entries) != 1 || manifest.Entries[0].Name != triage.ArtifactSlots[0].Name || manifest.Entries[0].MediaType != "application/json" {
				t.Fatalf("artifact manifest = %+v, want the one declared slot", manifest)
			}
			document, err := os.ReadFile(filepath.Join(repo, manifest.Entries[0].Path))
			if err != nil {
				t.Fatal(err)
			}
			if err := schemas.ValidateJSON("fanout-selection.schema.json", document); err != nil {
				t.Fatalf("selection %s violates the shipped envelope: %v", document, err)
			}
			var selection struct {
				Branches []struct{ Name, Reason string } `json:"branches"`
			}
			if err := json.Unmarshal(document, &selection); err != nil {
				t.Fatal(err)
			}
			got := []string{}
			for _, branch := range selection.Branches {
				if !optional[branch.Name] || branch.Reason == "" {
					t.Fatalf("selected %+v, want an optional lens with a reason", branch)
				}
				got = append(got, branch.Name)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("selected lenses = %v, want %v", got, tc.want)
			}
		})
	}
}

func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}
