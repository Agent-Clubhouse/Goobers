//go:build integration && linux

package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

const assembleTestTag = "v0.4.0-rc.1"

func releaseAssembleScript(t *testing.T) string {
	t.Helper()
	workflow := loadReleaseAuthorizationWorkflow(t)
	for _, step := range workflow.Jobs["assemble"].Steps {
		if step.Name == releaseAssembleStep {
			return step.Run
		}
	}
	t.Fatalf("assemble job lacks step %q", releaseAssembleStep)
	return ""
}

// assembleFixture lays out what the assemble job's downloads produce: the
// build's dist/ with its SHA256SUMS, and one directory per signer holding a
// re-packed archive whose bytes differ from the unsigned build.
func assembleFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"dist", "signed-darwin", "signed-windows", "runner-temp"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"install.sh", "feature-registry.json", "goobers-onboarding_" + assembleTestTag + ".zip"}
	for _, target := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
		names = append(names, "goobers_"+assembleTestTag+"_"+target+".tar.gz")
	}
	names = append(names, "goobers_"+assembleTestTag+"_windows_amd64.zip")
	var manifest strings.Builder
	for _, name := range names {
		data := []byte("unsigned " + name)
		writeAssembleFile(t, root, "dist/"+name, data)
		fmt.Fprintf(&manifest, "%x  %s\n", sha256.Sum256(data), name)
	}
	writeAssembleFile(t, root, "dist/SHA256SUMS", []byte(manifest.String()))
	// Not checksummed, but part of the build upload; it must be carried forward.
	writeAssembleFile(t, root, "dist/RELEASE_NOTES.md", []byte("draft notes"))
	for directory, signed := range assembleSigned() {
		for _, name := range signed {
			writeAssembleFile(t, root, directory+"/"+name, []byte("signed "+name))
		}
	}
	return root
}

func assembleSigned() map[string][]string {
	return map[string][]string{
		"signed-darwin": {
			"goobers_" + assembleTestTag + "_darwin_amd64.tar.gz",
			"goobers_" + assembleTestTag + "_darwin_arm64.tar.gz",
		},
		"signed-windows": {"goobers_" + assembleTestTag + "_windows_amd64.zip"},
	}
}

func writeAssembleFile(t *testing.T, root, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runAssemble(t *testing.T, root string) (string, error) {
	t.Helper()
	command := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", releaseAssembleScript(t))
	command.Dir = root
	command.Env = append(os.Environ(),
		"TAG="+assembleTestTag,
		"RUNNER_TEMP="+filepath.Join(root, "runner-temp"),
		"GITHUB_STEP_SUMMARY="+filepath.Join(root, "runner-temp", "summary.md"),
	)
	output, err := command.CombinedOutput()
	return string(output), err
}

func readManifest(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		digest, name, ok := strings.Cut(line, "  ")
		if !ok {
			t.Fatalf("manifest line is not GNU text-mode format: %q", line)
		}
		entries[name] = digest
	}
	return entries
}

func TestIntegrationReleaseAssembleMergesSignedArchivesOnce(t *testing.T) {
	// sha256sum ships with coreutils alongside the declared cp/head on Linux.
	testdep.Require(t, "bash", "find")
	root := assembleFixture(t)
	before := readManifest(t, filepath.Join(root, "dist", "SHA256SUMS"))
	if output, err := runAssemble(t, root); err != nil {
		t.Fatalf("assemble refused a well-formed signed set: %v\n%s", err, output)
	}
	after := readManifest(t, filepath.Join(root, "dist", "SHA256SUMS"))
	signed := make(map[string]bool)
	for _, names := range assembleSigned() {
		for _, name := range names {
			signed[name] = true
		}
	}
	if len(after) != len(before) {
		t.Fatalf("assembled manifest covers %d assets, want %d", len(after), len(before))
	}
	for name, digest := range before {
		data, err := os.ReadFile(filepath.Join(root, "dist", name))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); after[name] != got {
			t.Errorf("%s manifest digest %s does not match assembled bytes %s", name, after[name], got)
		}
		if signed[name] == (after[name] == digest) {
			t.Errorf("%s: signed=%v but digest changed=%v", name, signed[name], after[name] != digest)
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "dist", "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if !sort.StringsAreSorted(lines) {
		t.Error("assembled SHA256SUMS must stay sorted like the build's manifest")
	}
	if _, err := os.Stat(filepath.Join(root, "dist", "RELEASE_NOTES.md")); err != nil {
		t.Errorf("assemble dropped the build's unchecksummed notes: %v", err)
	}
}

func TestIntegrationReleaseAssembleFailsClosed(t *testing.T) {
	// sha256sum ships with coreutils alongside the declared cp/head on Linux.
	testdep.Require(t, "bash", "find")
	darwinArm := "goobers_" + assembleTestTag + "_darwin_arm64.tar.gz"
	windows := "goobers_" + assembleTestTag + "_windows_amd64.zip"
	type corruption struct {
		refusal string
		apply   func(t *testing.T, root string)
	}
	for name, test := range map[string]corruption{
		"damaged unsigned download": {"FAILED", func(t *testing.T, root string) {
			writeAssembleFile(t, root, "dist/install.sh", []byte("tampered"))
		}},
		"signer omitted an archive": {"must hold exactly", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "signed-darwin", darwinArm)); err != nil {
				t.Fatal(err)
			}
		}},
		"signer delivered an extra file": {"must hold exactly", func(t *testing.T, root string) {
			writeAssembleFile(t, root, "signed-windows/install.sh", []byte("signed install.sh"))
		}},
		"signer delivered another signer's archive": {"must hold exactly", func(t *testing.T, root string) {
			writeAssembleFile(t, root, "signed-windows/"+darwinArm, []byte("signed elsewhere"))
		}},
		"signed archive identical to unsigned build": {"signing did not change it", func(t *testing.T, root string) {
			writeAssembleFile(t, root, "signed-windows/"+windows, []byte("unsigned "+windows))
		}},
		"signed archive is a symlink": {"not a non-empty regular file", func(t *testing.T, root string) {
			path := filepath.Join(root, "signed-windows", windows)
			if err := os.Rename(path, filepath.Join(root, "outside")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(root, "outside"), path); err != nil {
				t.Fatal(err)
			}
		}},
		"signed archive is empty": {"not a non-empty regular file", func(t *testing.T, root string) {
			writeAssembleFile(t, root, "signed-darwin/"+darwinArm, nil)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			root := assembleFixture(t)
			test.apply(t, root)
			output, err := runAssemble(t, root)
			if err == nil {
				t.Fatalf("assemble accepted %s:\n%s", name, output)
			}
			if !strings.Contains(output, test.refusal) {
				t.Fatalf("assemble refused %s for the wrong reason; want %q:\n%s", name, test.refusal, output)
			}
		})
	}
}
