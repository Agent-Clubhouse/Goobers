package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var acquisitionCommands = map[string]*regexp.Regexp{
	"go-vulnerability-data":   regexp.MustCompile(`(?:\bgovulncheck(?:@[^\s]+)?|\$\(GOVULNCHECK\))\s+`),
	"kubernetes-schemas":      regexp.MustCompile(`(?:\bkubeconform(?:@[^\s]+)?|\$\(KUBECONFORM\))\s+`),
	"go-modules":              regexp.MustCompile(`(?:\bgo|\$\(GO\))\s+(?:run|test|build|vet|mod|install)\b`),
	"npm-packages":            regexp.MustCompile(`(?:\bnpm|\$\(NPM\))[^\n]*(?:\bci\b|\binstall\b|\bexec\b)`),
	"npm-advisories":          regexp.MustCompile(`(?:\bnpm|\$\(NPM\))[^\n]*\saudit(?:\s|$)`),
	"playwright-browsers":     regexp.MustCompile(`\bplaywright\s+install\b`),
	"browser-system-packages": regexp.MustCompile(`\bplaywright\s+(?:install-deps\b|install\b[^\n]*--with-deps)`),
	"apt-packages":            regexp.MustCompile(`\bapt-get\b`),
	"chocolatey-packages":     regexp.MustCompile(`\bchoco\s+install\b`),
	"python-packages":         regexp.MustCompile(`\bpip[3]?\s+install\b`),
	"maven-packages":          regexp.MustCompile(`\bmvn\b`),
	"nuget-packages":          regexp.MustCompile(`\bdotnet\s+(?:restore|build|test)\b`),
	"envtest-binaries":        regexp.MustCompile(`(?:\bsetup-envtest|\$\(SETUP_ENVTEST\))\s+use\b`),
	"direct-download":         regexp.MustCompile(`\b(?:curl|wget|Invoke-WebRequest|Invoke-RestMethod|Start-BitsTransfer)\b`),
}

func scriptAcquisitionSites(location, script string) []string {
	var lines []string
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Logging a command is not executing it. Preserve command
		// substitutions, which really can acquire data inside echo/printf.
		logging := strings.HasPrefix(trimmed, "echo ") || strings.HasPrefix(trimmed, "printf ") || strings.HasPrefix(trimmed, "Write-Output ")
		if logging && !strings.Contains(line, "$(") && !strings.Contains(line, "`") && !unquotedShellOperator(line) {
			continue
		}
		lines = append(lines, line)
	}
	text := strings.ReplaceAll(strings.Join(lines, "\n"), "\\\n", " ")
	var sites []string
	for kind, pattern := range acquisitionCommands {
		for index := range pattern.FindAllStringIndex(text, -1) {
			site := location
			if kind == "direct-download" {
				// Pin the invocation as well as its location: changing a URL
				// or computed download argument must force origin re-review.
				site += fmt.Sprintf("@%x", sha256.Sum256([]byte(text)))
			}
			if index > 0 {
				site += fmt.Sprintf("[%d]", index+1)
			}
			sites = append(sites, site+":"+kind)
		}
	}
	slices.Sort(sites)
	return sites
}

func unquotedShellOperator(line string) bool {
	var quote rune
	escaped := false
	for _, char := range line {
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
		case ';', '|', '&':
			return true
		}
	}
	return false
}

func provisioningAcquisitionSites(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, ".github/workflows/ci.yml"))
	if err != nil {
		return nil, err
	}
	var workflow ciWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return nil, err
	}
	var sites []string
	visited := make(map[string]bool)
	for name, job := range workflow.Jobs {
		found, err := stepAcquisitionSites(root, ".github/workflows/ci.yml#"+name, job.Steps, visited)
		if err != nil {
			return nil, err
		}
		sites = append(sites, found...)
	}
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return nil, err
	}
	// Include variable definitions: Make's pinned tool invocations live there,
	// not in the recipe that invokes the variable. Group recipes by target.
	section := "preamble"
	sections := make(map[string]string)
	for _, line := range strings.Split(string(makefile), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") || strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "\t") {
			section = strings.Fields(line)[0]
		}
		sections[section] += line + "\n"
	}
	for name, script := range sections {
		sites = append(sites, scriptAcquisitionSites("Makefile#"+name, script)...)
	}
	// npm run delegates to package scripts. Inspect those too, so adding a
	// downloader behind the existing portal-build check cannot evade the gate.
	packageData, err := os.ReadFile(filepath.Join(root, "portal", "package.json"))
	if err != nil {
		return nil, err
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(packageData, &pkg); err != nil {
		return nil, err
	}
	for name, script := range pkg.Scripts {
		sites = append(sites, scriptAcquisitionSites("portal/package.json#"+name, script)...)
	}
	slices.Sort(sites)
	return slices.Compact(sites), nil
}

func stepAcquisitionSites(root, location string, steps []ciStep, visited map[string]bool) ([]string, error) {
	var sites []string
	for i, step := range steps {
		name := step.Name
		if name == "" {
			name = fmt.Sprintf("step-%d", i+1)
		}
		prefix := location + "/" + name
		sites = append(sites, scriptAcquisitionSites(prefix, step.Run)...)
		if step.Uses == "" {
			continue
		}
		if !strings.HasPrefix(step.Uses, "./") {
			sites = append(sites, prefix+":github-actions")
			continue
		}
		local := filepath.ToSlash(filepath.Clean(step.Uses))
		if !strings.HasPrefix(local, ".github/actions/") {
			return nil, fmt.Errorf("unrecognized local CI action %q", step.Uses)
		}
		if visited[local] {
			continue
		}
		visited[local] = true
		data, err := os.ReadFile(filepath.Join(root, local, "action.yml"))
		if err != nil {
			return nil, err
		}
		var action struct {
			Runs struct {
				Using string   `yaml:"using"`
				Steps []ciStep `yaml:"steps"`
			} `yaml:"runs"`
		}
		if err := yaml.Unmarshal(data, &action); err != nil {
			return nil, err
		}
		if action.Runs.Using != "composite" {
			return nil, fmt.Errorf("local action %s requires acquisition inspection for %s runtime", local, action.Runs.Using)
		}
		found, err := stepAcquisitionSites(root, local+"/action.yml", action.Runs.Steps, visited)
		if err != nil {
			return nil, err
		}
		sites = append(sites, found...)
	}
	return sites, nil
}

func TestNewScriptAcquisitionCannotHideBehindExistingToolAllowlist(t *testing.T) {
	t.Parallel()
	for _, script := range []string{
		"curl -fsSL https://example.invalid/new-binary -o tool",
		"Invoke-WebRequest https://example.invalid/tool -OutFile tool.exe",
		"npm --prefix another-package ci",
		"choco install another-tool -y",
		"$(SETUP_ENVTEST) use 1.99 -p path",
	} {
		sites := scriptAcquisitionSites("new-step", script)
		if len(sites) == 0 || acquisitionDrift(nil, sites) == nil {
			t.Errorf("new acquisition not refused: %q => %v", script, sites)
		}
	}
}

func TestAcquisitionDiscoveryDetectsAnotherDownloadInRegisteredStep(t *testing.T) {
	t.Parallel()
	original := scriptAcquisitionSites("setup", "curl https://example.invalid/first -o first")
	changed := scriptAcquisitionSites("setup", "curl https://example.invalid/first -o first\ncurl https://example.invalid/second -o second")
	entry := acquisition{ID: "direct-download", What: "first binary", From: []string{"example.invalid"}, Offline: "no-offline-path", Sites: original}
	if err := acquisitionDrift([]acquisition{entry}, changed); err == nil {
		t.Fatal("adding another download to an existing declared step passed")
	}
}

func TestAcquisitionDiscoveryDetectsChangedDownloadOrigin(t *testing.T) {
	t.Parallel()
	original := scriptAcquisitionSites("setup", "curl https://original.invalid/tool -o tool")
	changed := scriptAcquisitionSites("setup", "curl https://different.invalid/tool -o tool")
	entry := acquisition{ID: "direct-download", What: "tool", From: []string{"original.invalid"}, Offline: "no-offline-path", Sites: original}
	if err := acquisitionDrift([]acquisition{entry}, changed); err == nil {
		t.Fatal("changing a declared download origin passed without manifest review")
	}
}

func TestScriptAcquisitionIgnoresCommentsAndNoAuditFlag(t *testing.T) {
	t.Parallel()
	sites := scriptAcquisitionSites("install", "# curl https://example.invalid\nnpm ci --no-audit --no-fund")
	if !slices.Equal(sites, []string{"install:npm-packages"}) {
		t.Fatalf("sites = %v", sites)
	}
}

func TestAcquisitionDiscoverySeparatesLoggingFromSubstitution(t *testing.T) {
	t.Parallel()
	if got := scriptAcquisitionSites("log", `echo "go mod download failed"`); len(got) != 0 {
		t.Fatalf("log text is not an acquisition: %v", got)
	}
	if got := scriptAcquisitionSites("fetch", `echo "$(curl https://example.invalid/tool)"`); len(got) != 1 {
		t.Fatalf("command substitution must be discovered: %v", got)
	}
	if got := scriptAcquisitionSites("fetch", `echo "preparing; please wait"; curl https://example.invalid/tool`); len(got) != 1 {
		t.Fatalf("command after logging must be discovered: %v", got)
	}
}

func TestAcquisitionDiscoveryReadsNewWorkflowAndLocalAction(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	files := map[string]string{
		"Makefile":            "ci:\n\tgo test ./...\n",
		"portal/package.json": `{"scripts":{"build":"npm install new-build-tool"}}`,
		".github/workflows/ci.yml": `jobs:
  new-job:
    steps:
      - name: New binary
        run: curl https://example.invalid/tool -o tool
      - name: Local setup
        uses: ./.github/actions/new-setup
`,
		".github/actions/new-setup/action.yml": `runs:
  using: composite
  steps:
    - name: New dependency
      shell: bash
      run: pip install new-package
`,
	}
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sites, err := provisioningAcquisitionSites(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		".github/actions/new-setup/action.yml/New dependency:python-packages",
		fmt.Sprintf(".github/workflows/ci.yml#new-job/New binary@%x:direct-download", sha256.Sum256([]byte("curl https://example.invalid/tool -o tool"))),
		"Makefile#ci::go-modules",
		"portal/package.json#build:npm-packages",
	}
	if !slices.Equal(sites, want) {
		t.Fatalf("discovered sites = %v, want %v", sites, want)
	}
	if acquisitionDrift(nil, sites) == nil {
		t.Fatal("new workflow/action acquisitions passed without declarations")
	}
}
