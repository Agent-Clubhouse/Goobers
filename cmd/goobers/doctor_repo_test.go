package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// fakeDoctorPolicyProvider is a seam substitute for GetRepoPolicy so these
// tests never make a real GitHub call.
type fakeDoctorPolicyProvider struct {
	result providers.RepoPolicyResult
	err    error
}

func (f fakeDoctorPolicyProvider) GetRepoPolicy(context.Context, providers.RepoPolicyRequest) (providers.RepoPolicyResult, error) {
	return f.result, f.err
}

// withFakeDoctorRepoProvider substitutes newDoctorGitHubProvider, restoring
// it after the test.
func withFakeDoctorRepoProvider(t *testing.T, result providers.RepoPolicyResult, err error) {
	t.Helper()
	orig := newDoctorGitHubProvider
	newDoctorGitHubProvider = func(string) providers.PolicyProvider {
		return fakeDoctorPolicyProvider{result: result, err: err}
	}
	t.Cleanup(func() { newDoctorGitHubProvider = orig })
}

// initDoctorRepoInstance scaffolds an instance root and declares the given
// policy on its first (scaffolded) repo.
func initDoctorRepoInstance(t *testing.T, policy *instance.RepoPolicyExpectation) string {
	t.Helper()
	t.Setenv("GOOBERS_GITHUB_TOKEN", "dummy-token-value")

	root := filepath.Join(t.TempDir(), "inst")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init code = %d, stderr = %q", code, stderr)
	}
	if policy == nil {
		return root
	}

	configFile := filepath.Join(root, "instance.yaml")
	cfg, err := instance.LoadConfig(configFile)
	if err != nil {
		t.Fatalf("load scaffolded config: %v", err)
	}
	if len(cfg.Repos) == 0 {
		t.Fatal("scaffolded config has no repos")
	}
	cfg.Repos[0].Policy = policy
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configFile, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return root
}

func TestDoctorRepoNoPolicyDeclaredSkips(t *testing.T) {
	root := initDoctorRepoInstance(t, nil)

	code, stdout, stderr := runArgs(t, "doctor", "--repo", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "nothing to check") {
		t.Fatalf("stdout = %q, want a skip notice", stdout)
	}
}

func TestDoctorRepoMatchesPolicyExitsZero(t *testing.T) {
	root := initDoctorRepoInstance(t, &instance.RepoPolicyExpectation{
		Branch:               "main",
		RequiredMergeMethod:  "squash",
		MergeQueueRequired:   true,
		RequiredStatusChecks: []string{"make ci"},
	})
	withFakeDoctorRepoProvider(t, providers.RepoPolicyResult{
		AllowedMergeMethods:  []providers.MergeMethod{providers.MergeMethodSquash},
		MergeQueuePolicy:     providers.MergePolicyMergeQueue,
		RequiredStatusChecks: []string{"make ci"},
		TokenScope:           providers.TokenScopeAvailable,
		TokenScopes:          []string{"repo", "workflow"},
	}, nil)

	code, stdout, stderr := runArgs(t, "doctor", "--repo", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "OK: matches declared policy") {
		t.Fatalf("stdout = %q, want a match confirmation", stdout)
	}
	if !strings.Contains(stdout, "token-scope=available") || !strings.Contains(stdout, "repo,workflow") {
		t.Fatalf("stdout = %q, want the available token scope reported", stdout)
	}
}

func TestDoctorRepoDriftExitsNonZero(t *testing.T) {
	root := initDoctorRepoInstance(t, &instance.RepoPolicyExpectation{
		RequiredMergeMethod:  "squash",
		MergeQueueRequired:   true,
		RequiredStatusChecks: []string{"make ci", "lint"},
	})
	withFakeDoctorRepoProvider(t, providers.RepoPolicyResult{
		AllowedMergeMethods:  []providers.MergeMethod{providers.MergeMethodSquash, providers.MergeMethodMerge},
		MergeQueuePolicy:     providers.MergePolicyDirect,
		RequiredStatusChecks: []string{"make ci"},
		TokenScope:           providers.TokenScopeUnavailable,
	}, nil)

	code, stdout, stderr := runArgs(t, "doctor", "--repo", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		`field="requiredMergeMethod"`,
		`declared="squash"`,
		`field="mergeQueueRequired"`,
		`field="requiredStatusChecks"`,
		"missing lint",
		"token-scope=unavailable",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestDoctorRepoJSONReport(t *testing.T) {
	root := initDoctorRepoInstance(t, &instance.RepoPolicyExpectation{
		RequiredMergeMethod: "merge",
	})
	withFakeDoctorRepoProvider(t, providers.RepoPolicyResult{
		AllowedMergeMethods: []providers.MergeMethod{providers.MergeMethodSquash},
		MergeQueuePolicy:    providers.MergePolicyDirect,
		TokenScope:          providers.TokenScopeUnavailable,
	}, nil)

	code, stdout, stderr := runArgs(t, "doctor", "--repo", "--report", "json", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stderr = %q", code, stderr)
	}
	var reports []doctorRepoReport
	if err := json.Unmarshal([]byte(stdout), &reports); err != nil {
		t.Fatalf("stdout is not the JSON report: %v\n%s", err, stdout)
	}
	if len(reports) != 1 || len(reports[0].Findings) != 1 {
		t.Fatalf("reports = %#v, want exactly one repo with one finding", reports)
	}
	if reports[0].Findings[0].Field != "requiredMergeMethod" {
		t.Fatalf("finding field = %q, want requiredMergeMethod", reports[0].Findings[0].Field)
	}
}

func TestDoctorRequiresExactlyOneMode(t *testing.T) {
	for _, args := range [][]string{
		{"doctor"},
		{"doctor", "--k8s", "--repo"},
		{"doctor", "--av-exclusions", "--repo"},
		{"doctor", "--av-exclusions", "--k8s"},
	} {
		code, _, stderr := runArgs(t, args...)
		if code != 2 {
			t.Fatalf("%v: code = %d, want 2", args, code)
		}
		if !strings.Contains(stderr, "exactly one of --k8s, --repo or --av-exclusions") {
			t.Fatalf("%v: stderr = %q, want the exactly-one-mode message", args, stderr)
		}
	}
}
