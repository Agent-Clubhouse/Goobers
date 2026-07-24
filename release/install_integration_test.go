//go:build integration && !windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testdep"
)

func TestIntegrationInstallScriptVerifiesAndRunsGuidedInit(t *testing.T) {
	testdep.Require(t, "sh")

	root := t.TempDir()
	fixtures := filepath.Join(root, "fixtures")
	tools := filepath.Join(root, "tools")
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}

	fakeBinary := filepath.Join(root, "goobers")
	fakeBinaryData := []byte("#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$GOOBERS_CALLS\"\n" +
		"if [ \"${1:-}\" = \"--version\" ]; then\n" +
		"  printf 'goobers v1.2.3 (test)\\n'\n" +
		"fi\n")
	if err := os.WriteFile(fakeBinary, fakeBinaryData, 0o755); err != nil {
		t.Fatal(err)
	}
	releaseRoot := filepath.Join(root, "release")
	releaseDocs := map[string][]byte{
		"README.md": []byte("# Goobers v1.2.3\n\n" +
			"The release installer already ran guided setup for `./my-instance`; do not initialize it again.\n\n" +
			"If you opened this README directly from an extracted archive instead:\n\n" +
			"```sh\ngoobers init --guided ./my-instance\n```\n"),
		"docs/RELEASE.md":           []byte("# Goobers v1.2.3 documentation\n"),
		"docs/guides/quickstart.md": []byte("# Quickstart v1.2.3\n"),
	}
	for name, data := range releaseDocs {
		path := filepath.Join(releaseRoot, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := packageArchive(
		Target{OS: "linux", Arch: "amd64"},
		"v1.2.3",
		fakeBinary,
		fixtures,
		releaseRoot,
	)
	if err != nil {
		t.Fatalf("packageArchive: %v", err)
	}
	manifest, err := checksumsManifest([]string{archive})
	if err != nil {
		t.Fatalf("checksumsManifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "SHA256SUMS"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, filepath.Join(tools, "uname"), `#!/bin/sh
case "${1:-}" in
  -s) printf 'Linux\n' ;;
  -m) printf 'x86_64\n' ;;
  *) exit 2 ;;
esac
`)
	writeExecutable(t, filepath.Join(tools, "curl"), `#!/bin/sh
set -eu
output=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output=$2; shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
printf '%s\n' "$url" >> "$CURL_CALLS"
cp "$FIXTURE_DIR/${url##*/}" "$output"
`)

	scriptPath := filepath.Join(root, installScriptFile)
	if err := os.WriteFile(scriptPath, []byte(installScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("sh", "-n", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("installer shell syntax: %v\n%s", err, output)
	}

	installDir := filepath.Join(root, "bin")
	dataDir := filepath.Join(root, "data")
	instancePath := filepath.Join(root, "instance with space")
	curlCalls := filepath.Join(root, "curl-calls")
	goobersCalls := filepath.Join(root, "goobers-calls")
	for _, version := range []string{"main", "v1.2", "v1.2.3-rc1", "v01.2.3"} {
		cmd := exec.Command("sh", scriptPath, version, instancePath)
		if output, err := cmd.CombinedOutput(); err == nil ||
			!strings.Contains(string(output), "exact stable tag") {
			t.Fatalf("install %q result = %v, output = %s", version, err, output)
		}
	}

	cmd := exec.Command("sh", scriptPath, "v1.2.3", instancePath)
	cmd.Env = append(os.Environ(),
		"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FIXTURE_DIR="+fixtures,
		"CURL_CALLS="+curlCalls,
		"GOOBERS_CALLS="+goobersCalls,
		"GOOBERS_INSTALL_DIR="+installDir,
		"XDG_DATA_HOME="+dataDir,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("installer: %v\n%s", err, output)
	}

	installed, err := os.ReadFile(filepath.Join(installDir, "goobers"))
	if err != nil {
		t.Fatalf("installed binary: %v", err)
	}
	if !bytes.Equal(installed, fakeBinaryData) {
		t.Fatal("installed binary differs from the checksummed archive")
	}
	installedDocsDir := filepath.Join(dataDir, "goobers", "v1.2.3")
	for name, want := range releaseDocs {
		got, err := os.ReadFile(filepath.Join(installedDocsDir, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("installed documentation %s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("installed documentation %s = %q, want %q", name, got, want)
		}
	}
	installedReadme, err := os.ReadFile(filepath.Join(installedDocsDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	assertSubstringsInOrder(
		t,
		"installed README onboarding",
		string(installedReadme),
		"The release installer already ran guided setup",
		"do not initialize it again",
		"directly from an extracted archive instead",
		"goobers init --guided ./my-instance",
	)
	calls, err := os.ReadFile(goobersCalls)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--version", "init --guided " + instancePath} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("binary calls lack %q:\n%s", want, calls)
		}
	}
	downloads, err := os.ReadFile(curlCalls)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"https://github.com/Agent-Clubhouse/Goobers/releases/download/v1.2.3/goobers_v1.2.3_linux_amd64.tar.gz",
		"https://github.com/Agent-Clubhouse/Goobers/releases/download/v1.2.3/SHA256SUMS",
	} {
		if !strings.Contains(string(downloads), want) {
			t.Errorf("downloads lack %q:\n%s", want, downloads)
		}
	}

	if err := os.WriteFile(archive, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	failedInstallDir := filepath.Join(root, "failed-bin")
	failedDataDir := filepath.Join(root, "failed-data")
	cmd = exec.Command("sh", scriptPath, "v1.2.3", instancePath)
	cmd.Env = append(os.Environ(),
		"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FIXTURE_DIR="+fixtures,
		"CURL_CALLS="+curlCalls,
		"GOOBERS_CALLS="+goobersCalls,
		"GOOBERS_INSTALL_DIR="+failedInstallDir,
		"XDG_DATA_HOME="+failedDataDir,
	)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "checksum mismatch") {
		t.Fatalf("corrupt archive result = %v, output = %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(failedInstallDir, "goobers")); !os.IsNotExist(err) {
		t.Fatalf("checksum failure installed a binary: %v", err)
	}
	if _, err := os.Stat(filepath.Join(failedDataDir, "goobers", "v1.2.3")); !os.IsNotExist(err) {
		t.Fatalf("checksum failure installed documentation: %v", err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
