package packaging

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestWindowsImagePinnedInputs(t *testing.T) {
	var dependencies struct {
		WindowsBase    string                       `json:"windowsBase"`
		WindowsVersion string                       `json:"windowsVersion"`
		MinGit         struct{ URL, SHA256 string } `json:"mingit"`
		ZoneInfo       struct{ URL, SHA256 string } `json:"zoneinfo"`
	}
	if err := json.Unmarshal(readWindowsImageFile(t, "dependencies.json"), &dependencies); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^mcr\.microsoft\.com/windows/servercore:ltsc2022@sha256:[a-f0-9]{64}$`).MatchString(dependencies.WindowsBase) || !strings.HasPrefix(dependencies.WindowsVersion, "10.0.20348.") {
		t.Fatalf("unqualified Windows Server 2022 base: %+v", dependencies)
	}
	dockerfile := string(readWindowsImageFile(t, "Dockerfile"))
	if !strings.Contains(dockerfile, "ARG WINDOWS_BASE_IMAGE="+dependencies.WindowsBase+"\n") {
		t.Fatal("Dockerfile and dependency manifest base pins disagree")
	}
	for name, hash := range map[string]string{"mingit": dependencies.MinGit.SHA256, "zoneinfo": dependencies.ZoneInfo.SHA256} {
		if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(hash) {
			t.Errorf("%s checksum is not pinned: %q", name, hash)
		}
	}
	if !strings.HasPrefix(dependencies.MinGit.URL, "https://github.com/git-for-windows/git/releases/download/") || !strings.HasSuffix(dependencies.MinGit.URL, "-64-bit.zip") || strings.Contains(dependencies.MinGit.URL, "busybox") {
		t.Fatalf("expected regular upstream x64 MinGit: %s", dependencies.MinGit.URL)
	}
	goMod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	version := regexp.MustCompile(`(?m)^go (\S+)$`).FindStringSubmatch(string(goMod))
	if len(version) != 2 || dependencies.ZoneInfo.URL != "https://raw.githubusercontent.com/golang/go/go"+version[1]+"/lib/time/zoneinfo.zip" {
		t.Fatalf("zoneinfo source must match repository Go toolchain: %s", dependencies.ZoneInfo.URL)
	}
	user := strings.LastIndex(dockerfile, "\nUSER ContainerUser\n")
	verify := strings.LastIndex(dockerfile, "\nRUN C:\\Goobers\\Verify-Image.ps1\n")
	if user < 0 || verify < user {
		t.Fatal("native contract must execute after switching to ContainerUser")
	}
	for _, forbidden := range []string{"COPY . ", "ADD ", "Invoke-WebRequest", "curl ", "npm ", "choco "} {
		if strings.Contains(dockerfile, forbidden) {
			t.Errorf("prepared image Dockerfile contains %q", forbidden)
		}
	}
}

// This executes the real input verifier when PowerShell is available. The
// fixtures are deliberately inert bytes; native executable stamps are a later
// image-build check. No container, registry or credentials are needed here.
func TestWindowsImageInputVerification(t *testing.T) {
	powerShell := findImageTestPowerShell(t)
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

func TestWindowsImageReleaseStampPreservesMetadata(t *testing.T) {
	powerShell := findImageTestPowerShell(t)
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

func TestWindowsImagePowerShellSyntax(t *testing.T) {
	powerShell := findImageTestPowerShell(t)
	// Parse all scripts even where their Windows APIs cannot execute. Windows
	// PowerShell 5.1 remains a separate native validation requirement.
	for _, name := range []string{"Verify-Inputs.ps1", "Configure-Image.ps1", "Verify-Image.ps1", "Release-Metadata.ps1"} {
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

func readWindowsImageFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("docker/windows", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func findImageTestPowerShell(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"powershell", "pwsh"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	t.Skip("PowerShell unavailable; input verification and script syntax require powershell or pwsh")
	return ""
}
