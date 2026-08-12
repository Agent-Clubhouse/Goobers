package main

// Tests for validatereality.go, each reproducing a 2026-08-08 cold-start
// probe: the exact config shape that validated clean before the check
// existed, asserted to produce the intended finding now — plus a negative
// proving the corrected config (and the shipped canonical shapes) stay
// clean.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// stubRepositoryRealityChecks replaces the --check-repos reality seams with
// hermetic fakes: the repository claims repoLabels, reports workflowCount CI
// workflows, and lists matchingItems open work items each carrying every one
// of repoLabels (so any selector drawn from repoLabels matches them).
func stubRepositoryRealityChecks(t *testing.T, repoLabels []string, workflowCount, matchingItems int) {
	t.Helper()
	originalLabels := targetRepositoryLabels
	originalWorkflows := targetRepositoryWorkflowCount
	originalLister := validateRealityLister
	t.Cleanup(func() {
		targetRepositoryLabels = originalLabels
		targetRepositoryWorkflowCount = originalWorkflows
		validateRealityLister = originalLister
	})
	targetRepositoryLabels = func(context.Context, instance.RepoRef, string) ([]string, error) {
		return append([]string(nil), repoLabels...), nil
	}
	targetRepositoryWorkflowCount = func(context.Context, instance.RepoRef, string) (int, error) {
		return workflowCount, nil
	}
	items := make([]providers.WorkItem, matchingItems)
	for i := range items {
		items[i] = providers.WorkItem{
			ID:     fmt.Sprintf("%d", i+1),
			Labels: append([]string(nil), repoLabels...),
		}
	}
	validateRealityLister = func(string) repoWorkItemLister {
		return fakeRealityLister{items: items}
	}
}

type fakeRealityLister struct{ items []providers.WorkItem }

func (f fakeRealityLister) ListWorkItems(context.Context, providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
	return f.items, nil
}

// TestValidateWarnsOnUnclaimedRunnerCapability reproduces cold-start dotnet
// #7 and the swift `totally-made-up-toolchain@42` probe: a gaggle requiring
// capabilities the instance's runner never claims validated clean while the
// scheduler would refuse placement of every run.
func TestValidateWarnsOnUnclaimedRunnerCapability(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cap")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code=%d stderr=%q", code, stderr)
	}
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	appendToFile(t, gagglePath, "  requiredCapabilities:\n    - nosuchtoolchain@42\n    - python@3.12\n")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate code=%d, want 0 (warning, not error); stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		`WARNING CAP003 Gaggle/example: requires runner capability "nosuchtoolchain@42"`,
		"runner.capabilities in instance.yaml does not claim it",
		"refuse to place every run",
		`requires runner capability "python@3.12"`,
		"exact string match",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("validate stdout missing %q:\n%s", want, stdout)
		}
	}
	// The prober-family note names the family list only for a token outside
	// every family: nosuchtoolchain gets it, python does not.
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "nosuchtoolchain@42") && !strings.Contains(line, "prober families (dotnet, go, java, node, os, python)") {
			t.Errorf("unknown-family token line missing the prober-family note: %s", line)
		}
		if strings.Contains(line, "python@3.12") && strings.Contains(line, "prober families") {
			t.Errorf("known-family token must not carry the family note: %s", line)
		}
	}

	// Negative: claiming the same tokens in instance.yaml runner.capabilities
	// clears both warnings — exact-match semantics, same as the scheduler.
	replaceInFile(t, filepath.Join(root, "instance.yaml"),
		"runner: {}",
		"runner:\n  capabilities:\n  - nosuchtoolchain@42\n  - python@3.12")
	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate after claiming: code=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "CAP003") {
		t.Fatalf("claimed capabilities must not warn:\n%s", stdout)
	}
}

func TestAppendMaxOpenPRWarnings(t *testing.T) {
	tests := []struct {
		name        string
		project     apiv1.RepoRef
		repos       []instance.RepoRef
		maxOpenPRs  int32
		wantWarning bool
		wantText    []string
	}{
		{
			name:        "ADO project cannot enforce cap",
			project:     apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme", Project: "store", Name: "web"},
			repos:       []instance.RepoRef{{Provider: "ado", Owner: "acme", Project: "store", Name: "web"}},
			maxOpenPRs:  2,
			wantWarning: true,
			wantText: []string{
				"cannot be enforced for ADO project repository",
				`"ado/acme/store/web"`,
				"cap counts GitHub pull requests",
			},
		},
		{
			name:        "empty project with sole ADO repository cannot enforce cap",
			project:     apiv1.RepoRef{},
			repos:       []instance.RepoRef{{Provider: "ado", Owner: "acme", Project: "store", Name: "web"}},
			maxOpenPRs:  2,
			wantWarning: true,
			wantText: []string{
				"cannot be enforced for ADO project repository",
				`"acme/store/web"`,
				"cap counts GitHub pull requests",
			},
		},
		{
			name:        "unconfigured GitHub project names actual binding",
			project:     apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "other", Name: "site"},
			repos:       []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "web"}},
			maxOpenPRs:  3,
			wantWarning: true,
			wantText: []string{
				`binds to project repository "github/other/site"`,
				"no configured binding",
				"count remains unknown",
			},
		},
		{
			name:    "empty project names first repository fallback",
			project: apiv1.RepoRef{},
			repos: []instance.RepoRef{
				{Provider: "github", Owner: "acme", Name: "web"},
				{Provider: "github", Owner: "other", Name: "site"},
			},
			maxOpenPRs:  3,
			wantWarning: true,
			wantText: []string{
				"has no project repository binding",
				`binds to instance repos[0] repository "acme/web"`,
			},
		},
		{
			name:       "configured GitHub project is enforceable",
			project:    apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
			repos:      []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "web"}},
			maxOpenPRs: 1,
		},
		{
			name:       "cap disabled",
			project:    apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "acme", Project: "store", Name: "web"},
			repos:      []instance.RepoRef{{Provider: "ado", Owner: "acme", Project: "store", Name: "web"}},
			maxOpenPRs: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set := &instance.ConfigSet{
				Gaggles: []apiv1.Gaggle{{
					ObjectMeta: metav1.ObjectMeta{Name: "example"},
					Spec:       apiv1.GaggleSpec{Project: tc.project},
				}},
				Workflows: []apiv1.Workflow{{
					ObjectMeta: metav1.ObjectMeta{Name: "implementation"},
					Spec: apiv1.WorkflowSpec{
						Gaggle:    "example",
						Readiness: apiv1.ReadinessConditions{MaxOpenPRs: tc.maxOpenPRs},
					},
				}},
			}
			report := &validate.Report{}
			warnings := appendStaticRealityWarnings("", "config", &instance.Config{Repos: tc.repos}, set, report)
			if got := len(warnings); (got == 1) != tc.wantWarning {
				t.Fatalf("warning count = %d, want warning %t: %#v", got, tc.wantWarning, warnings)
			}
			if !tc.wantWarning {
				return
			}
			warning := warnings[0]
			if warning.warning.Code != validate.WarningMaxOpenPRsUnenforceable {
				t.Errorf("warning code = %q, want %q", warning.warning.Code, validate.WarningMaxOpenPRsUnenforceable)
			}
			if warning.path != "/spec/readiness/maxOpenPRs" {
				t.Errorf("warning path = %q", warning.path)
			}
			for _, want := range tc.wantText {
				if !strings.Contains(warning.warning.Explanation, want) {
					t.Errorf("warning missing %q: %s", want, warning.warning.Explanation)
				}
			}
		})
	}
}

// wireLocalCIGate rewires the scaffolded default-implement workflow so
// open-pr flows into a deterministic local-ci stage gated by a status-equals
// gate — the canonical local-gate shape of cold-start swift #3 — with the
// failure branch routed to branches[fail] and continueOnError controlled by
// the caller.
func wireLocalCIGate(t *testing.T, root, failTarget, continueOnError string) {
	t.Helper()
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, workflowPath, "        - opened", "        - opened\n      next: local-ci")
	appendToFile(t, workflowPath, fmt.Sprintf(`    - name: local-ci
      type: deterministic
      goal: Run the local CI-equivalent gate.
      run:
        command: ["make", "ci"]
%s      next: local-gate
  gates:
    - name: local-gate
      evaluator: automated
      automated:
        check: status-equals
      branches:
        pass: ""
        fail: %s
`, continueOnError, failTarget))
}

// TestValidateWarnsOnGateCompletionHidingFailure encodes the verified form
// of cold-start swift #3: the gate's fail branch is NOT dead (the runner
// always delivers a failed stage's status to a `next` gate), but a fail
// branch routed to workflow completion without continueOnError on the
// feeding stage cannot complete — the run terminates failed (#849).
func TestValidateWarnsOnGateCompletionHidingFailure(t *testing.T) {
	t.Run("fail branch to completion without continueOnError warns", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "gate")
		if code, _, stderr := runArgs(t, "init", root); code != 0 {
			t.Fatalf("init: code=%d stderr=%q", code, stderr)
		}
		wireLocalCIGate(t, root, `""`, "")
		code, stdout, stderr := runArgs(t, "validate", root)
		if code != 0 {
			t.Fatalf("validate code=%d, want 0 (warning, not error); stdout=%q stderr=%q", code, stdout, stderr)
		}
		for _, want := range []string{
			"WARNING WF018 Workflow/default-implement",
			`gate "local-gate" branch "fail"`,
			`stage "local-ci" does not set`,
			"terminates failed instead of completed",
			"set continueOnError: true",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("validate stdout missing %q:\n%s", want, stdout)
			}
		}
	})

	t.Run("continueOnError true stays clean", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "gate-coe")
		if code, _, stderr := runArgs(t, "init", root); code != 0 {
			t.Fatalf("init: code=%d stderr=%q", code, stderr)
		}
		wireLocalCIGate(t, root, `""`, "      continueOnError: true\n")
		code, stdout, _ := runArgs(t, "validate", root)
		if code != 0 || strings.Contains(stdout, "WF018") {
			t.Fatalf("tolerated failure must not warn (code=%d):\n%s", code, stdout)
		}
	})

	t.Run("shipped repass shape stays clean", func(t *testing.T) {
		// fail routed to a real state (the canonical local-gate -> implement
		// repass and the parking stages of the shipped references) is
		// reachable and correct without continueOnError — it must never warn.
		root := filepath.Join(t.TempDir(), "gate-repass")
		if code, _, stderr := runArgs(t, "init", root); code != 0 {
			t.Fatalf("init: code=%d stderr=%q", code, stderr)
		}
		wireLocalCIGate(t, root, `"@abort"`, "")
		code, stdout, _ := runArgs(t, "validate", root)
		if code != 0 || strings.Contains(stdout, "WF018") {
			t.Fatalf("failure branch to a terminal/parking target must not warn (code=%d):\n%s", code, stdout)
		}
	})
}

// TestValidateCheckReposSelectorLabelReality reproduces cold-start python
// #1/#7 and swift #10: the scaffolded `goobers` selector against a
// repository that carries none of the referenced labels, and the claim
// mirror --claim applies.
func TestValidateCheckReposSelectorLabelReality(t *testing.T) {
	root := filepath.Join(t.TempDir(), "reality")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code=%d stderr=%q", code, stderr)
	}
	t.Setenv("GOOBERS_GITHUB_TOKEN", "test-token")
	original := targetRepositoryReachable
	t.Cleanup(func() { targetRepositoryReachable = original })
	targetRepositoryReachable = func(context.Context, instance.RepoRef, string, credentials.StoreResolver) error {
		return nil
	}
	originalSize := targetRepositorySize
	t.Cleanup(func() { targetRepositorySize = originalSize })
	targetRepositorySize = func(context.Context, instance.RepoRef, string) (int64, error) { return 100, nil }

	t.Run("missing labels warn with provenance", func(t *testing.T) {
		stubRepositoryRealityChecks(t, []string{"bug"}, 1, 1)
		code, stdout, stderr := runArgs(t, "validate", "--check-repos", root)
		if code != 0 {
			t.Fatalf("advisory reality findings must not change the exit code: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		for _, want := range []string{
			`Gaggle/example spec.backlog.labels selects label "goobers", which does not exist on the repository`,
			"the loop would claim nothing",
			`task "query-backlog" inputs.trustLabel selects label "goobers"`,
			`(--claim's claim mirror) applies label "goobers:claimed"`,
			"GitHub rejects applying labels that do not exist",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout missing %q:\n%s", want, stdout)
			}
		}
		if strings.Contains(stdout, "match 0 of") {
			t.Errorf("zero-match warning is redundant when the label itself is missing:\n%s", stdout)
		}
	})

	t.Run("existing labels with zero matching items warn", func(t *testing.T) {
		stubRepositoryRealityChecks(t, []string{"goobers", "goobers:claimed"}, 1, 0)
		code, stdout, stderr := runArgs(t, "validate", "--check-repos", root)
		if code != 0 {
			t.Fatalf("advisory reality findings must not change the exit code: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		for _, want := range []string{
			"Gaggle/example backlog selectors (goobers) match 0 of",
			"claim nothing",
			"indistinguishable from an idle daemon",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout missing %q:\n%s", want, stdout)
			}
		}
		if strings.Contains(stdout, "SELECTOR001") || strings.Contains(stdout, "SELECTOR003") {
			t.Errorf("existing labels must not produce existence warnings:\n%s", stdout)
		}
	})

	t.Run("matching reality stays clean", func(t *testing.T) {
		stubRepositoryRealityChecks(t, []string{"goobers", "goobers:claimed"}, 1, 3)
		code, stdout, stderr := runArgs(t, "validate", "--check-repos", root)
		if code != 0 {
			t.Fatalf("validate code=%d stderr=%q", code, stderr)
		}
		for _, forbidden := range []string{"SELECTOR001", "SELECTOR002", "SELECTOR003", "CIPOLL001"} {
			if strings.Contains(stdout, forbidden) {
				t.Errorf("clean reality must not warn (%s):\n%s", forbidden, stdout)
			}
		}
		if !strings.Contains(stdout, "OK: instance.yaml valid") {
			t.Errorf("expected OK banner:\n%s", stdout)
		}
	})
}

// TestValidateCheckReposCIPollReality reproduces the swift probe of
// cold-start README item 1: a full ci-poll workflow aimed at a repository
// with zero CI validates clean even under --check-repos, though every run
// would park at the gate timeout.
func TestValidateCheckReposCIPollReality(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cipoll")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code=%d stderr=%q", code, stderr)
	}
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, workflowPath, "        - opened", "        - opened\n      next: ci-poll")
	appendToFile(t, workflowPath, `    - name: ci-poll
      type: deterministic
      goal: Poll the PR's CI checks until they conclude.
      run:
        command: ["goobers", "ci-poll"]
      inputs:
        kind: "ci-poll"
      inputsFrom:
        prNumber: prNumber
      capabilities:
        - provider:pr:write
      next: ci-gate
  gates:
    - name: ci-gate
      evaluator: automated
      automated:
        check: ci-status
      branches:
        pass: ""
        fail: "@escalate"
        timeout: "@escalate"
`)
	t.Setenv("GOOBERS_GITHUB_TOKEN", "test-token")
	original := targetRepositoryReachable
	t.Cleanup(func() { targetRepositoryReachable = original })
	targetRepositoryReachable = func(context.Context, instance.RepoRef, string, credentials.StoreResolver) error {
		return nil
	}
	originalSize := targetRepositorySize
	t.Cleanup(func() { targetRepositorySize = originalSize })
	targetRepositorySize = func(context.Context, instance.RepoRef, string) (int64, error) { return 100, nil }

	t.Run("zero CI workflows warn", func(t *testing.T) {
		stubRepositoryRealityChecks(t, []string{"goobers", "goobers:claimed"}, 0, 1)
		code, stdout, stderr := runArgs(t, "validate", "--check-repos", root)
		if code != 0 {
			t.Fatalf("advisory reality findings must not change the exit code: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		for _, want := range []string{
			`workflow "default-implement" stage "ci-poll" polls the pull request's CI checks`,
			"no GitHub Actions workflows",
			"park at the CI gate's timeout branch",
			"local-gate-only",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout missing %q:\n%s", want, stdout)
			}
		}
	})

	t.Run("repository with CI stays clean", func(t *testing.T) {
		stubRepositoryRealityChecks(t, []string{"goobers", "goobers:claimed"}, 4, 1)
		code, stdout, stderr := runArgs(t, "validate", "--check-repos", root)
		if code != 0 {
			t.Fatalf("validate code=%d stderr=%q", code, stderr)
		}
		if strings.Contains(stdout, "CIPOLL001") || strings.Contains(stdout, "no GitHub Actions workflows") {
			t.Errorf("a repository with CI must not warn:\n%s", stdout)
		}
	})

	t.Run("routed credential without Actions read warns", func(t *testing.T) {
		stubRepositoryRealityChecks(t, []string{"goobers", "goobers:claimed"}, 4, 1)
		targetRepositoryWorkflowCount = func(context.Context, instance.RepoRef, string) (int, error) {
			return 0, fmt.Errorf("GET actions/workflows failed: status 404: Not Found")
		}
		code, stdout, stderr := runArgs(t, "validate", "--check-repos", root)
		if code != 0 {
			t.Fatalf("advisory credential finding must not change the exit code: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		for _, want := range []string{
			"routed credential could not read",
			"GitHub Actions workflows",
			"status 404",
			"grant Actions: Read",
			"correct its credential route",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout missing %q:\n%s", want, stdout)
			}
		}

		code, stdout, stderr = runArgs(t, "validate", "--json", "--check-repos", root)
		if code != 0 || stderr != "" {
			t.Fatalf("JSON validate code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		var envelope diagnosticsEnvelope
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatalf("decode diagnostics: %v\n%s", err, stdout)
		}
		found := false
		for _, finding := range envelope.Findings {
			if finding.Code == "CIPOLL001" && strings.Contains(finding.Message, "routed credential could not read") {
				found = true
			}
		}
		if !found {
			t.Fatalf("diagnostics missing credential-read CIPOLL001: %+v", envelope.Findings)
		}
	})
}

// TestRepositoryRealityNotCheckedForNonGitHub asserts the no-false-confidence
// note: providers whose label/tag reality is not cheaply fetchable (ADO tags)
// get an explicit "not checked" line instead of silent apparent coverage.
func TestRepositoryRealityNotCheckedForNonGitHub(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{{
		Provider: "ado",
		Owner:    "org",
		Project:  "proj",
		Name:     "repo",
	}}}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{
		ObjectMeta: metav1.ObjectMeta{Name: "boards"},
		Spec: apiv1.GaggleSpec{
			Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "proj", Labels: []string{"goobers"}},
		},
	}}}
	var out strings.Builder
	checkRepositoryReality("", "config", cfg, set, nil, &out)
	if !strings.Contains(out.String(), `selector/CI reality not checked for provider "ado"`) {
		t.Fatalf("expected the not-checked note for ado, got:\n%s", out.String())
	}
}

// TestGitHubRealityFetchers exercises the provider-backed readers against a
// local server: label listing, the Actions workflow count, and non-200 failure.
func TestGitHubRealityFetchers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/labels"):
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("Authorization = %q, want bearer token", got)
			}
			_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "goobers"}, {"name": "bug"}})
		case strings.HasSuffix(r.URL.Path, "/actions/workflows"):
			_ = json.NewEncoder(w).Encode(map[string]int{"total_count": 3})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider := providers.NewGitHubProvider("test-token")
	provider.BaseURL = server.URL

	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "web"}
	labels, err := provider.RepositoryLabelNames(context.Background(), repo)
	if err != nil || len(labels) != 2 || labels[0] != "goobers" {
		t.Fatalf("labels = %v, %v", labels, err)
	}
	count, err := provider.ActionsWorkflowCount(context.Background(), repo)
	if err != nil || count != 3 {
		t.Fatalf("workflow count = %d, %v", count, err)
	}
	failing := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer failing.Close()
	provider.BaseURL = failing.URL
	if _, err := provider.RepositoryLabelNames(context.Background(), repo); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}
