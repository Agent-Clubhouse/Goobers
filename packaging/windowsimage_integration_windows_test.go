//go:build integration && windows

package packaging

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// This executes the real input verifier under Windows PowerShell 5.1. The
// fixtures are deliberately inert bytes; native executable stamps are a later
// image-build check. No container, registry or credentials are needed here.
func TestIntegrationWindowsImageInputVerification(t *testing.T) {
	testdep.Require(t, "powershell.exe")
	powerShell := windowsImagePowerShell(t)
	script, err := filepath.Abs("docker/windows/Verify-Inputs.ps1")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"valid", "binary tamper", "dependency tamper", "duplicate checksum", "wrong platform", "short commit", "missing commit"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string][]byte{
				"goobers.exe":          []byte("inert goobers fixture"),
				"goobers-operator.exe": []byte("inert operator fixture"),
				"mingit.zip":           []byte("inert git archive fixture"),
				"zoneinfo.zip":         []byte("inert timezone fixture"),
			}
			metadata := map[string]any{"schemaVersion": 1, "kind": "goobers-base-build-inputs", "platform": "windows/amd64", "version": "v0.4.0-rc.1", "commit": strings.Repeat("a", 40), "date": "2026-09-07T00:00:00Z"}
			if scenario == "wrong platform" {
				metadata["platform"] = "linux/amd64"
			}
			if scenario == "short commit" {
				metadata["commit"] = "abcdef012345"
			}
			if scenario == "missing commit" {
				metadata["commit"] = ""
			}
			files["release.json"], err = json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			pins := map[string]any{}
			for _, name := range []string{"mingit", "zoneinfo"} {
				pins[name] = map[string]string{"sha256": fmt.Sprintf("%x", sha256.Sum256(files[name+".zip"]))}
			}
			files["dependencies.json"], err = json.Marshal(pins)
			if err != nil {
				t.Fatal(err)
			}
			var sums strings.Builder
			for _, name := range []string{"goobers.exe", "goobers-operator.exe", "release.json"} {
				fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(files[name]), name)
			}
			if scenario == "duplicate checksum" {
				fmt.Fprintf(&sums, "%x  goobers.exe\n", sha256.Sum256(files["goobers.exe"]))
			}
			files["SHA256SUMS"] = []byte(sums.String())
			if scenario == "binary tamper" {
				files["goobers.exe"] = []byte("tampered")
			}
			if scenario == "dependency tamper" {
				files["mingit.zip"] = []byte("tampered")
			}
			for name, data := range files {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", script)
			cmd.Dir = dir
			output, runErr := cmd.CombinedOutput()
			if (runErr == nil) != (scenario == "valid" || scenario == "short commit") {
				t.Fatalf("verifier = %v, output=%s", runErr, output)
			}
		})
	}
}

func TestIntegrationWindowsImageReleaseStampPreservesMetadata(t *testing.T) {
	testdep.Require(t, "powershell.exe")
	powerShell := windowsImagePowerShell(t)
	helper, err := filepath.Abs("docker/windows/Release-Metadata.ps1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, date, mismatch string
	}{
		{name: "UTC", date: "2026-09-07T00:00:00Z"},
		{name: "positive offset", date: "2026-09-07T05:30:00+05:30"},
		{name: "negative offset and fraction", date: "2026-09-06T17:00:00.1234567-07:00"},
		{name: "commit mismatch", date: "2026-09-07T00:00:00Z", mismatch: "commit"},
		{name: "date spelling mismatch", date: "2026-09-07T00:00:00Z", mismatch: "date"},
		{name: "version mismatch", date: "2026-09-07T00:00:00Z", mismatch: "version"},
		{name: "platform mismatch", date: "2026-09-07T00:00:00Z", mismatch: "platform"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			metadata := map[string]any{"schemaVersion": 1, "kind": "goobers-base-build-inputs", "platform": "windows/amd64", "version": "v0.4.0-rc.1", "commit": "abcdef012345", "date": tc.date}
			raw, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			metadataPath := filepath.Join(dir, "release.json")
			if err := os.WriteFile(metadataPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			date, version, commit, platform := tc.date, "v0.4.0-rc.1", "abcdef012345", "windows/amd64"
			switch tc.mismatch {
			case "commit":
				commit = "aaaaaaaaaaaa"
			case "date":
				date = "2026-09-07T00:00:00+00:00"
			case "version":
				version = "v0.4.0-beta.2"
			case "platform":
				platform = "linux/amd64"
			}
			output := fmt.Sprintf("operator %s (commit %s, built %s, go1.26.6 %s)", version, commit, date, platform)
			script := `param([string]$Helper, [string]$Metadata, [string]$ExpectedDate, [string]$VersionOutput)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
. $Helper
$release = Read-ImageRelease -Path $Metadata
if ($release.date -isnot [string] -or $release.date -cne $ExpectedDate) { throw 'Build date text was changed by JSON parsing' }
Assert-ImageReleaseStamp -Release $release -Output $VersionOutput -Binary 'operator'
`
			scriptPath := filepath.Join(dir, "verify-stamp.ps1")
			if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			result, runErr := exec.Command(powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", scriptPath, helper, metadataPath, tc.date, output).CombinedOutput()
			if (runErr == nil) != (tc.mismatch == "") {
				t.Fatalf("stamp verification = %v: %s", runErr, result)
			}
			if tc.mismatch != "" && !strings.Contains(string(result), "release stamp mismatch") {
				t.Fatalf("expected stamp mismatch, got %s", result)
			}
		})
	}
}

func TestIntegrationWindowsImagePowerShellSyntax(t *testing.T) {
	testdep.Require(t, "powershell.exe")
	powerShell := windowsImagePowerShell(t)
	// Parse every script with the required native runtime before image builds
	// exercise their Windows API and ContainerUser behavior.
	for _, name := range []string{"Verify-Inputs.ps1", "Configure-Image.ps1", "Verify-Image.ps1", "Release-Metadata.ps1", "Smoke-Image.ps1"} {
		path, err := filepath.Abs(filepath.Join("docker/windows", name))
		if err != nil {
			t.Fatal(err)
		}
		command := "$tokens = $null; $parseErrors = $null; [System.Management.Automation.Language.Parser]::ParseFile('" + strings.ReplaceAll(path, "'", "''") + "', [ref]$tokens, [ref]$parseErrors) | Out-Null; if ($parseErrors.Count -ne 0) { $parseErrors | Out-String | Write-Error; exit 1 }"
		output, err := exec.Command(powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command).CombinedOutput()
		if err != nil {
			t.Fatalf("%s parse: %v: %s", name, err, output)
		}
	}
}

func windowsImagePowerShell(t *testing.T) string {
	t.Helper()
	const executable = "powershell.exe"
	command := `$PSVersionTable.PSEdition + '/' + $PSVersionTable.PSVersion.Major + '.' + $PSVersionTable.PSVersion.Minor`
	output, err := exec.Command(executable, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command).CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "Desktop/5.1" {
		t.Fatalf("image verifier requires Windows PowerShell Desktop/5.1, got %q (%v)", output, err)
	}
	return executable
}
