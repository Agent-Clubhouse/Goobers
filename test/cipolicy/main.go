// Command cipolicy selects and verifies the GitHub Actions validation profile.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"
)

const (
	fullProfile   = "full"
	portalProfile = "portal-only"
)

var backendJobs = []string{
	"cmdgoobers-growth", "deploy-reference", "lint", "darwin-build",
	"unit", "unit-linux-coverage", "shipped", "deadcode", "windows-smoke",
	"vulnerability-scan", "integration", "sandbox", "linux-validation",
}

var commitSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

type gitRunner func(args ...string) ([]byte, error)

type jobResult struct {
	Result  string            `json:"result"`
	Outputs map[string]string `json:"outputs"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, func(args ...string) ([]byte, error) {
		output, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, output)
		}
		return output, nil
	}, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, git gitRunner, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/cipolicy classify|gate")
		return 2
	}
	switch args[0] {
	case "classify":
		profile, reason, err := classify(getenv("CI_EVENT_NAME"), getenv("BASE_SHA"), getenv("HEAD_SHA"), git)
		if err != nil {
			profile = fullProfile
			reason = fmt.Sprintf("classification failed; retaining full validation: %v", err)
			_, _ = fmt.Fprintf(stderr, "::warning::%q\n", reason)
		}
		_, _ = fmt.Fprintf(stderr, "CI profile: %s (%q)\n", profile, reason)
		if summary := getenv("GITHUB_STEP_SUMMARY"); summary != "" {
			if err := writeSummary(summary, profile); err != nil {
				_, _ = fmt.Fprintf(stderr, "cipolicy: %v\n", err)
				return 1
			}
		}
		if _, err := fmt.Fprintf(stdout, "profile=%s\n", profile); err != nil {
			_, _ = fmt.Fprintf(stderr, "cipolicy: write profile: %v\n", err)
			return 1
		}
		return 0
	case "gate":
		var needs map[string]jobResult
		if err := json.Unmarshal([]byte(getenv("CI_NEEDS")), &needs); err != nil {
			_, _ = fmt.Fprintf(stderr, "cipolicy: decode required job results: %v\n", err)
			return 1
		}
		if err := validateGate(getenv("CI_EVENT_NAME"), needs); err != nil {
			_, _ = fmt.Fprintf(stderr, "cipolicy: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "Required %s CI gates passed.\n", needs["scope"].Outputs["profile"])
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "cipolicy: unknown command %q\n", args[0])
		return 2
	}
}

func classify(event, base, head string, git gitRunner) (string, string, error) {
	if event != "pull_request" {
		return fullProfile, "only pull requests qualify for portal-only validation", nil
	}
	if !commitSHA.MatchString(base) || !commitSHA.MatchString(head) {
		return "", "", fmt.Errorf("base and head must be full commit SHAs")
	}
	output, err := git("merge-base", base, head)
	if err != nil {
		return "", "", err
	}
	mergeBase := strings.TrimSpace(string(output))
	if !commitSHA.MatchString(mergeBase) {
		return "", "", fmt.Errorf("merge-base did not return one full commit SHA")
	}
	// Disabling rename detection exposes both deleted and added paths, so a
	// backend file moved into portal/ still requires the full gate.
	output, err = git("diff", "--raw", "-z", "--no-abbrev", "--no-ext-diff", "--no-renames", mergeBase, head, "--")
	if err != nil {
		return "", "", err
	}
	return classifyDiff(string(output))
}

func classifyDiff(raw string) (string, string, error) {
	if raw == "" {
		return fullProfile, "empty diff", nil
	}
	fields := strings.Split(raw, "\x00")
	if fields[len(fields)-1] != "" || len(fields)%2 != 1 {
		return "", "", fmt.Errorf("incomplete NUL-delimited raw diff")
	}
	for i := 0; i < len(fields)-1; i += 2 {
		header := strings.Fields(fields[i])
		if len(header) != 5 || !strings.HasPrefix(header[0], ":") ||
			!commitSHA.MatchString(header[2]) || !commitSHA.MatchString(header[3]) {
			return "", "", fmt.Errorf("invalid raw diff header %q", fields[i])
		}
		regular, err := regularChangeModes(header[4], strings.TrimPrefix(header[0], ":"), header[1])
		if err != nil {
			return "", "", err
		}
		if !regular {
			return fullProfile, "unsupported change status, symlink, submodule, or file mode", nil
		}
		name := fields[i+1]
		if !strings.HasPrefix(name, "portal/") || name == "portal/" ||
			path.Clean(name) != name || strings.ContainsAny(name, "\\\r\n") {
			return fullProfile, "change outside regular portal paths: " + name, nil
		}
		if strings.HasSuffix(name, ".go") || path.Base(name) == "go.mod" || path.Base(name) == "go.sum" {
			return fullProfile, "Go source or module change: " + name, nil
		}
	}
	return portalProfile, "all changed files are regular portal files", nil
}

func regularChangeModes(status, oldMode, newMode string) (bool, error) {
	switch status {
	case "A":
		if oldMode != "000000" || newMode == "000000" {
			return false, fmt.Errorf("inconsistent added-file modes")
		}
	case "D":
		if oldMode == "000000" || newMode != "000000" {
			return false, fmt.Errorf("inconsistent deleted-file modes")
		}
	case "M":
		if oldMode == "000000" || newMode == "000000" {
			return false, fmt.Errorf("inconsistent modified-file modes")
		}
	default:
		return false, nil
	}
	for _, mode := range []string{oldMode, newMode} {
		if mode != "000000" && mode != "100644" && mode != "100755" {
			return false, nil
		}
	}
	return true, nil
}

func requiredJobs() []string {
	return append([]string{"scope", "preflight", "checks"}, backendJobs...)
}

func validateGate(event string, needs map[string]jobResult) error {
	if event != "pull_request" && event != "merge_group" && event != "workflow_dispatch" {
		return fmt.Errorf("unsupported required-gate event %q", event)
	}
	expected := requiredJobs()
	if len(needs) != len(expected) {
		return fmt.Errorf("required job set has %d entries, want %d", len(needs), len(expected))
	}
	scope := needs["scope"]
	if scope.Result != "success" {
		return fmt.Errorf("scope did not pass (result: %q)", scope.Result)
	}
	profile := scope.Outputs["profile"]
	if profile != fullProfile && profile != portalProfile {
		return fmt.Errorf("missing or unknown CI profile %q", profile)
	}
	if profile == portalProfile && event != "pull_request" {
		return fmt.Errorf("portal-only profile is not permitted for %s", event)
	}
	var failures []string
	for _, name := range expected {
		job, exists := needs[name]
		if !exists {
			failures = append(failures, "missing job "+name)
			continue
		}
		if job.Result == "success" {
			continue
		}
		if profile == portalProfile && slices.Contains(backendJobs, name) && job.Result == "skipped" {
			continue
		}
		failures = append(failures, fmt.Sprintf("%s did not pass (result: %q)", name, job.Result))
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

func writeSummary(filename, profile string) error {
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open CI summary: %w", err)
	}
	_, writeErr := fmt.Fprintf(file, "## CI profile: `%s`\n\n", profile)
	if writeErr == nil && profile == portalProfile {
		_, writeErr = fmt.Fprintln(file, "Preflight and portal UX/component, real-daemon, contract, embedded-asset, and package checks remain required. Only the explicitly listed backend jobs are excluded:\n\n`"+strings.Join(backendJobs, "`, `")+"`")
	}
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write CI summary: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close CI summary: %w", closeErr)
	}
	return nil
}
