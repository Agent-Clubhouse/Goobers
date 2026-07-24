package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteInstallScript(t *testing.T) {
	path, err := writeInstallScript(t.TempDir())
	if err != nil {
		t.Fatalf("writeInstallScript: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, installScript) {
		t.Fatal("written installer differs from embedded installer")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("installer mode = %o, want 755", info.Mode().Perm())
	}
}

func TestInstallScriptVerifiesAndRunsGuidedInit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the release installer supports Linux and macOS")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is unavailable")
	}

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
	archive, err := packageArchive(Target{OS: "linux", Arch: "amd64"}, "v1.2.3", fakeBinary, fixtures)
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
	if err := os.WriteFile(scriptPath, installScript, 0o755); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(sh, "-n", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("installer shell syntax: %v\n%s", err, output)
	}

	installDir := filepath.Join(root, "bin")
	instancePath := filepath.Join(root, "instance with space")
	curlCalls := filepath.Join(root, "curl-calls")
	goobersCalls := filepath.Join(root, "goobers-calls")
	cmd := exec.Command(sh, scriptPath, "main", instancePath)
	if output, err := cmd.CombinedOutput(); err == nil ||
		!strings.Contains(string(output), "exact stable tag") {
		t.Fatalf("moving-ref install result = %v, output = %s", err, output)
	}

	cmd = exec.Command(sh, scriptPath, "v1.2.3", instancePath)
	cmd.Env = append(os.Environ(),
		"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FIXTURE_DIR="+fixtures,
		"CURL_CALLS="+curlCalls,
		"GOOBERS_CALLS="+goobersCalls,
		"GOOBERS_INSTALL_DIR="+installDir,
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
	cmd = exec.Command(sh, scriptPath, "v1.2.3", instancePath)
	cmd.Env = append(os.Environ(),
		"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FIXTURE_DIR="+fixtures,
		"CURL_CALLS="+curlCalls,
		"GOOBERS_CALLS="+goobersCalls,
		"GOOBERS_INSTALL_DIR="+failedInstallDir,
	)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "checksum mismatch") {
		t.Fatalf("corrupt archive result = %v, output = %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(failedInstallDir, "goobers")); !os.IsNotExist(err) {
		t.Fatalf("checksum failure installed a binary: %v", err)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
