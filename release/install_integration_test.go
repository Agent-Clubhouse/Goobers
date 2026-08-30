//go:build integration && !windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationInstallScriptVerifiesInstallAndPointsToGettingStarted(t *testing.T) {
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
		"printf '%s %s\\n' \"${0##*/}\" \"$*\" >> \"$GOOBERS_CALLS\"\n" +
		"if [ \"${1:-}\" = \"--version\" ]; then\n" +
		"  printf 'goobers v1.2.3 (test)\\n'\n" +
		"fi\n")
	if err := os.WriteFile(fakeBinary, fakeBinaryData, 0o755); err != nil {
		t.Fatal(err)
	}
	releaseRoot := filepath.Join(root, "release")
	releaseDocs := map[string][]byte{
		"README.md": []byte("# Goobers v1.2.3\n\n" +
			"The release installer installs the binary and documentation only. Start setup " +
			"with `goobers init --guided` after installation.\n\n" +
			"If you opened this README directly from an extracted archive instead:\n\n" +
			"```sh\ngoobers-v1.2.3 init --guided\n```\n"),
		"docs/RELEASE.md":           []byte("# Goobers v1.2.3 documentation\n"),
		"docs/guides/quickstart.md": []byte("# Quickstart v1.2.3\n"),
		"onboarding/manifest.json":  []byte("{\"release\":{\"version\":\"v1.2.3\"}}\n"),
		"onboarding/templates/quickstart@v1/manifest.yaml": []byte(
			"apiVersion: goobers.dev/v1alpha1\nkind: Manifest\n",
		),
		"onboarding/samples/getting-started-task-api@1.0.0/seed-issues.json": []byte(
			"{\"sample\":{\"version\":\"1.0.0\"}}\n",
		),
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
	curlCalls := filepath.Join(root, "curl-calls")
	goobersCalls := filepath.Join(root, "goobers-calls")
	instancePath := filepath.Join(root, "instance with space")
	for _, version := range []string{"main", "v1.2", "v1.2.3-rc1", "v01.2.3"} {
		cmd := exec.Command("sh", scriptPath, version, instancePath)
		if output, err := cmd.CombinedOutput(); err == nil ||
			!strings.Contains(string(output), "exact stable tag") {
			t.Fatalf("install %q result = %v, output = %s", version, err, output)
		}
	}

	cmd := exec.Command("sh", scriptPath, "v1.2.3")
	cmd.Env = append(os.Environ(),
		"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FIXTURE_DIR="+fixtures,
		"CURL_CALLS="+curlCalls,
		"GOOBERS_CALLS="+goobersCalls,
		"GOOBERS_INSTALL_DIR="+installDir,
		"XDG_DATA_HOME="+dataDir,
	)
	installOutput, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer: %v\n%s", err, installOutput)
	}
	if !strings.Contains(string(installOutput), "Set up your repository:") ||
		!strings.Contains(string(installOutput), "init --guided") {
		t.Errorf("installer output lacks Getting Started guidance:\n%s", installOutput)
	}

	installed, err := os.ReadFile(filepath.Join(installDir, "goobers"))
	if err != nil {
		t.Fatalf("installed binary: %v", err)
	}
	if !bytes.Equal(installed, fakeBinaryData) {
		t.Fatal("installed binary differs from the checksummed archive")
	}
	versionedInstalled, err := os.ReadFile(filepath.Join(installDir, "goobers-v1.2.3"))
	if err != nil {
		t.Fatalf("versioned installed binary: %v", err)
	}
	if !bytes.Equal(versionedInstalled, fakeBinaryData) {
		t.Fatal("versioned installed binary differs from the checksummed archive")
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
		"The release installer installs the binary and documentation only",
		"goobers init --guided",
		"directly from an extracted archive instead",
		"goobers-v1.2.3 init --guided",
	)
	calls, err := os.ReadFile(goobersCalls)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"goobers --version"} {
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

	// Install-only is the default: no init invocation, next steps printed.
	plainInstallDir := filepath.Join(root, "plain-bin")
	plainDataDir := filepath.Join(root, "plain-data")
	plainCurlCalls := filepath.Join(root, "plain-curl-calls")
	plainGoobersCalls := filepath.Join(root, "plain-goobers-calls")
	cmd = exec.Command("sh", scriptPath, "v1.2.3")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FIXTURE_DIR="+fixtures,
		"CURL_CALLS="+plainCurlCalls,
		"GOOBERS_CALLS="+plainGoobersCalls,
		"GOOBERS_INSTALL_DIR="+plainInstallDir,
		"XDG_DATA_HOME="+plainDataDir,
	)
	plainOutput, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install-only default: %v\n%s", err, plainOutput)
	}
	plainCalls, err := os.ReadFile(plainGoobersCalls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plainCalls), "init") {
		t.Errorf("default install ran init:\n%s", plainCalls)
	}
	assertSubstringsInOrder(
		t,
		"install-only next steps",
		string(plainOutput),
		"Next steps",
		"init --demo ./demo-instance",
		"run demo ./demo-instance",
		"init --guided",
	)

	// A bare positional path is rejected before any download.
	cmd = exec.Command("sh", scriptPath, "v1.2.3", instancePath)
	cmd.Env = append(os.Environ(),
		"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FIXTURE_DIR="+fixtures,
		"CURL_CALLS="+filepath.Join(root, "rejected-curl-calls"),
		"GOOBERS_CALLS="+filepath.Join(root, "rejected-goobers-calls"),
		"GOOBERS_INSTALL_DIR="+filepath.Join(root, "rejected-bin"),
		"XDG_DATA_HOME="+filepath.Join(root, "rejected-data"),
	)
	if output, err := cmd.CombinedOutput(); err == nil ||
		!strings.Contains(string(output), "unexpected argument") {
		t.Fatalf("bare instance path result = %v, output = %s", err, output)
	}

	// The removed guided option is rejected before any download or installation.
	removedGuidedInstallDir := filepath.Join(root, "removed-guided-bin")
	removedGuidedCurlCalls := filepath.Join(root, "removed-guided-curl-calls")
	cmd = exec.Command("sh", scriptPath, "v1.2.3", "--guided", instancePath)
	cmd.Env = append(os.Environ(),
		"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FIXTURE_DIR="+fixtures,
		"CURL_CALLS="+removedGuidedCurlCalls,
		"GOOBERS_CALLS="+filepath.Join(root, "removed-guided-goobers-calls"),
		"GOOBERS_INSTALL_DIR="+removedGuidedInstallDir,
		"XDG_DATA_HOME="+filepath.Join(root, "removed-guided-data"),
	)
	removedGuidedOutput, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(removedGuidedOutput), "--guided has been removed") {
		t.Fatalf("removed guided option result = %v, output = %s", err, removedGuidedOutput)
	}
	if _, err := os.Stat(removedGuidedCurlCalls); !os.IsNotExist(err) {
		t.Fatalf("removed guided option downloaded release assets: %v", err)
	}
	if _, err := os.Stat(removedGuidedInstallDir); !os.IsNotExist(err) {
		t.Fatalf("removed guided option wrote installation files: %v", err)
	}

	fakeBinaryV124 := filepath.Join(root, "goobers-v1.2.4-source")
	fakeBinaryV124Data := []byte(strings.Replace(string(fakeBinaryData), "v1.2.3", "v1.2.4", 1))
	if err := os.WriteFile(fakeBinaryV124, fakeBinaryV124Data, 0o755); err != nil {
		t.Fatal(err)
	}
	archiveV124, err := packageArchive(
		Target{OS: "linux", Arch: "amd64"},
		"v1.2.4",
		fakeBinaryV124,
		fixtures,
		releaseRoot,
	)
	if err != nil {
		t.Fatalf("package v1.2.4 archive: %v", err)
	}
	manifest, err = checksumsManifest([]string{archive, archiveV124})
	if err != nil {
		t.Fatalf("checksums for sequential install: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "SHA256SUMS"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("sh", scriptPath, "v1.2.4")
	cmd.Env = append(os.Environ(),
		"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FIXTURE_DIR="+fixtures,
		"CURL_CALLS="+curlCalls,
		"GOOBERS_CALLS="+goobersCalls,
		"GOOBERS_INSTALL_DIR="+installDir,
		"XDG_DATA_HOME="+dataDir,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sequential v1.2.4 install: %v\n%s", err, output)
	}
	for name, want := range map[string][]byte{
		"goobers":        fakeBinaryV124Data,
		"goobers-v1.2.3": fakeBinaryData,
		"goobers-v1.2.4": fakeBinaryV124Data,
	} {
		got, err := os.ReadFile(filepath.Join(installDir, name))
		if err != nil {
			t.Errorf("read sequentially installed %s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("sequentially installed %s has unexpected bytes", name)
		}
	}

	if err := os.WriteFile(archive, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	failedInstallDir := filepath.Join(root, "failed-bin")
	failedDataDir := filepath.Join(root, "failed-data")
	cmd = exec.Command("sh", scriptPath, "v1.2.3")
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
	if _, err := os.Stat(filepath.Join(failedInstallDir, "goobers-v1.2.3")); !os.IsNotExist(err) {
		t.Fatalf("checksum failure installed a versioned binary: %v", err)
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
