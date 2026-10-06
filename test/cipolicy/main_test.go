package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	baseSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func rawChange(status, oldMode, newMode, name string) string {
	return ":" + oldMode + " " + newMode + " " + baseSHA + " " + headSHA + " " + status + "\x00" + name + "\x00"
}

func TestClassifyDiff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, diff, want string
		wantErr          bool
	}{
		{"portal source", rawChange("M", "100644", "100644", "portal/src/styles.css"), portalProfile, false},
		{"portal dependency", rawChange("M", "100644", "100644", "portal/package-lock.json"), portalProfile, false},
		{"portal tests", rawChange("A", "000000", "100644", "portal/e2e/new.spec.ts"), portalProfile, false},
		{"portal deletion", rawChange("D", "100644", "000000", "portal/src/old.tsx"), portalProfile, false},
		{"portal rename", rawChange("D", "100644", "000000", "portal/src/old.tsx") + rawChange("A", "000000", "100644", "portal/src/new.tsx"), portalProfile, false},
		{"backend to portal rename", rawChange("D", "100644", "000000", "internal/old.ts") + rawChange("A", "000000", "100644", "portal/src/new.ts"), fullProfile, false},
		{"portal to backend rename", rawChange("D", "100644", "000000", "portal/src/old.ts") + rawChange("A", "000000", "100644", "internal/new.ts"), fullProfile, false},
		{"mixed", rawChange("M", "100644", "100644", "portal/src/App.tsx") + rawChange("M", "100644", "100644", "go.mod"), fullProfile, false},
		{"workflow", rawChange("M", "100644", "100644", ".github/workflows/ci.yml"), fullProfile, false},
		{"backend deletion", rawChange("D", "100644", "000000", "internal/x.go"), fullProfile, false},
		{"similar prefix", rawChange("M", "100644", "100644", "portal-other/x.ts"), fullProfile, false},
		{"Go in portal", rawChange("A", "000000", "100644", "portal/new.go"), fullProfile, false},
		{"module in portal", rawChange("A", "000000", "100644", "portal/go.mod"), fullProfile, false},
		{"symlink", rawChange("A", "000000", "120000", "portal/link"), fullProfile, false},
		{"deleted symlink", rawChange("D", "120000", "000000", "portal/link"), fullProfile, false},
		{"submodule", rawChange("M", "160000", "160000", "portal/submodule"), fullProfile, false},
		{"type change", rawChange("T", "100644", "120000", "portal/link"), fullProfile, false},
		{"traversal", rawChange("A", "000000", "100644", "portal/../go.mod"), fullProfile, false},
		{"backslash", rawChange("A", "000000", "100644", `portal/a\b`), fullProfile, false},
		{"newline", rawChange("A", "000000", "100644", "portal/a\nb"), fullProfile, false},
		{"empty", "", fullProfile, false},
		{"truncated", strings.TrimSuffix(rawChange("M", "100644", "100644", "portal/x.ts"), "\x00"), "", true},
		{"invalid header", "invalid\x00portal/x.ts\x00", "", true},
		{"inconsistent addition", rawChange("A", "100644", "100644", "portal/x.ts"), "", true},
		{"inconsistent deletion", rawChange("D", "100644", "100644", "portal/x.ts"), "", true},
		{"inconsistent modification", rawChange("M", "000000", "100644", "portal/x.ts"), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := classifyDiff(tc.diff)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("profile=%q error=%v, want %q error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestClassifyUsesCompleteMergeBaseDiff(t *testing.T) {
	t.Parallel()
	var calls [][]string
	git := func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "merge-base" {
			return []byte(baseSHA + "\n"), nil
		}
		return []byte(rawChange("M", "100644", "100644", "portal/src/styles.css")), nil
	}
	profile, _, err := classify("pull_request", baseSHA, headSHA, git)
	if err != nil || profile != portalProfile {
		t.Fatalf("classify: profile=%q error=%v", profile, err)
	}
	want := [][]string{
		{"merge-base", baseSHA, headSHA},
		{"diff", "--raw", "-z", "--no-abbrev", "--no-ext-diff", "--no-renames", baseSHA, headSHA, "--"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls=%q, want %q", calls, want)
	}
	for _, event := range []string{"push", "merge_group", "workflow_dispatch", ""} {
		profile, _, err := classify(event, "", "", func(...string) ([]byte, error) {
			t.Fatal("non-PR classification must not inspect a diff")
			return nil, nil
		})
		if err != nil || profile != fullProfile {
			t.Fatalf("%s: profile=%q error=%v", event, profile, err)
		}
	}
}

func TestClassifierDoesNotTruncateLargeDiffs(t *testing.T) {
	t.Parallel()
	var diff strings.Builder
	for i := 0; i < 400; i++ {
		diff.WriteString(rawChange("M", "100644", "100644", fmt.Sprintf("portal/src/file-%d.ts", i)))
	}
	if profile, _, err := classifyDiff(diff.String()); err != nil || profile != portalProfile {
		t.Fatalf("large portal diff: profile=%q error=%v", profile, err)
	}
	diff.WriteString(rawChange("M", "100644", "100644", "internal/backend.go"))
	if profile, _, err := classifyDiff(diff.String()); err != nil || profile != fullProfile {
		t.Fatalf("backend change after 400 portal files: profile=%q error=%v", profile, err)
	}
}

func TestClassificationErrorsRetainFullCIWithWarning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, base string
		git        gitRunner
	}{
		{"missing ref", "", func(...string) ([]byte, error) { t.Fatal("invalid SHA must not reach git"); return nil, nil }},
		{"missing merge base", baseSHA, func(...string) ([]byte, error) { return nil, errors.New("missing commit") }},
		{"ambiguous merge base", baseSHA, func(...string) ([]byte, error) { return []byte(baseSHA + "\n" + headSHA), nil }},
		{"diff failure", baseSHA, func(args ...string) ([]byte, error) {
			if args[0] == "merge-base" {
				return []byte(baseSHA), nil
			}
			return nil, errors.New("diff failed")
		}},
		{"malformed diff", baseSHA, func(args ...string) ([]byte, error) {
			if args[0] == "merge-base" {
				return []byte(baseSHA), nil
			}
			return []byte("partial"), nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"CI_EVENT_NAME": "pull_request", "BASE_SHA": tc.base, "HEAD_SHA": headSHA}
			var stdout, stderr bytes.Buffer
			code := run([]string{"classify"}, func(key string) string { return env[key] }, tc.git, &stdout, &stderr)
			if code != 0 || stdout.String() != "profile=full\n" || !strings.Contains(stderr.String(), "::warning::") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func gateResults(profile string) map[string]jobResult {
	needs := make(map[string]jobResult)
	for _, name := range requiredJobs() {
		result := "success"
		if profile == portalProfile && slices.Contains(backendJobs, name) {
			result = "skipped"
		}
		needs[name] = jobResult{Result: result}
	}
	needs["scope"] = jobResult{Result: "success", Outputs: map[string]string{"profile": profile}}
	return needs
}

func TestGateProfiles(t *testing.T) {
	t.Parallel()
	for _, event := range []string{"pull_request", "merge_group", "workflow_dispatch"} {
		if err := validateGate(event, gateResults(fullProfile)); err != nil {
			t.Fatalf("full %s: %v", event, err)
		}
		err := validateGate(event, gateResults(portalProfile))
		if (err == nil) != (event == "pull_request") {
			t.Fatalf("portal-only %s: %v", event, err)
		}
	}
	if err := validateGate("push", gateResults(fullProfile)); err == nil {
		t.Fatal("required gate must reject unsupported events")
	}
}

func TestGateRejectsUnexpectedJobResults(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{fullProfile, portalProfile} {
		for _, name := range requiredJobs() {
			for _, result := range []string{"skipped", "failure", "cancelled", "", "unknown"} {
				t.Run(profile+"/"+name+"/"+result, func(t *testing.T) {
					needs := gateResults(profile)
					job := needs[name]
					job.Result = result
					needs[name] = job
					allowed := profile == portalProfile && slices.Contains(backendJobs, name) && result == "skipped"
					if err := validateGate("pull_request", needs); (err == nil) != allowed {
						t.Fatalf("allowed=%v error=%v", allowed, err)
					}
				})
			}
			needs := gateResults(profile)
			delete(needs, name)
			if err := validateGate("pull_request", needs); err == nil {
				t.Fatalf("%s accepts missing %s", profile, name)
			}
		}
	}
}

func TestGateRejectsMissingProfileOrChangedJobSet(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"", "other"} {
		if err := validateGate("pull_request", gateResults(profile)); err == nil {
			t.Fatalf("accepts profile %q", profile)
		}
	}
	needs := gateResults(portalProfile)
	needs["unclassified-job"] = jobResult{Result: "skipped"}
	if err := validateGate("pull_request", needs); err == nil {
		t.Fatal("accepts unknown job")
	}
	needs = gateResults(fullProfile)
	delete(needs, "lint")
	needs["unclassified-job"] = jobResult{Result: "success"}
	if err := validateGate("pull_request", needs); err == nil {
		t.Fatal("accepts a replacement for a required job")
	}
}

func TestGateCommand(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"", "null", "{}", "not-json"} {
		var stdout, stderr bytes.Buffer
		env := map[string]string{"CI_EVENT_NAME": "pull_request", "CI_NEEDS": input}
		if code := run([]string{"gate"}, func(key string) string { return env[key] }, nil, &stdout, &stderr); code != 1 || stderr.Len() == 0 {
			t.Fatalf("input=%q code=%d stderr=%q", input, code, stderr.String())
		}
	}
	data, err := json.Marshal(gateResults(portalProfile))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	env := map[string]string{"CI_EVENT_NAME": "pull_request", "CI_NEEDS": string(data)}
	if code := run([]string{"gate"}, func(key string) string { return env[key] }, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestPolicyMatchesWorkflow(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}

	var workflow struct {
		Jobs map[string]struct {
			If      string            `yaml:"if"`
			Needs   []string          `yaml:"needs"`
			Outputs map[string]string `yaml:"outputs"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	got := slices.Clone(workflow.Jobs["required-ci"].Needs)
	want := requiredJobs()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("required-ci needs=%q, policy jobs=%q", got, want)
	}
	for _, name := range backendJobs {
		job := workflow.Jobs[name]
		if !slices.Contains(job.Needs, "scope") || !strings.Contains(job.If, "needs.scope.outputs.profile == 'full'") {
			t.Errorf("%s must depend on scope and require full validation", name)
		}
	}
	for _, name := range []string{"preflight", "checks"} {
		if strings.Contains(workflow.Jobs[name].If, "outputs.profile") {
			t.Errorf("%s must retain coverage in both profiles", name)
		}
	}
	if workflow.Jobs["scope"].Outputs["profile"] != "${{ steps.classify.outputs.profile }}" {
		t.Fatal("scope must expose the classifier's profile")
	}
}

func TestClassifierWritesProfileAndSummary(t *testing.T) {
	t.Parallel()
	summary := filepath.Join(t.TempDir(), "summary.md")
	env := map[string]string{
		"CI_EVENT_NAME": "pull_request", "BASE_SHA": baseSHA, "HEAD_SHA": headSHA,
		"GITHUB_STEP_SUMMARY": summary,
	}
	git := func(args ...string) ([]byte, error) {
		if args[0] == "merge-base" {
			return []byte(baseSHA), nil
		}
		return []byte(rawChange("M", "100644", "100644", "portal/src/styles.css")), nil
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"classify"}, func(key string) string { return env[key] }, git, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "profile=portal-only\n" {
		t.Fatalf("unexpected output %q", stdout.String())
	}
	data, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range backendJobs {
		if !strings.Contains(string(data), "`"+name+"`") {
			t.Errorf("summary omits excluded job %s", name)
		}
	}
	env["GITHUB_STEP_SUMMARY"] = filepath.Join(t.TempDir(), "missing", "summary.md")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"classify"}, func(key string) string { return env[key] }, git, &stdout, &stderr); code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("summary failure: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
