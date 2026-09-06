package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
)

func TestValidateSurfacesResolvedLargeRepoPreset(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{{
		Provider:  "github",
		Owner:     "acme",
		Name:      "monolith",
		LargeRepo: true,
	}}}
	cfg.ResolveLargeRepoPresets()
	var out strings.Builder
	printResolvedLargeRepoPresets(&out, cfg.Repos)
	for _, want := range []string{
		"workspace=pinned",
		"serial=true",
		"defaultStageTimeout=4h",
		"stalledRunTimeout=6h",
		"maxRunDuration=24h",
		"pathLength=enabled (max 260)",
		"mirrorRefspec=heads+tags",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("resolved preset output missing %q: %s", want, out.String())
		}
	}
}

func TestValidateForeignLayoutDiagnosticsAndExitCodes(t *testing.T) {
	type mutation func(t *testing.T, root string)
	tests := []struct {
		name   string
		mutate mutation
		code   int
		want   string
	}{
		{name: "valid", code: 0, want: "OK: instance.yaml valid; config/ valid"},
		{
			name: "unbound workflow",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
				replaceInFile(t, path, "  gaggle: example", "  gaggle: ghost")
			},
			code: 1,
			want: `gaggles/example/workflows/default-implement.yaml Workflow/default-implement: spec.gaggle names "ghost", but no Gaggle/ghost definition was found`,
		},
		{
			name: "manifest gaggle mismatch",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, "config", "manifest.yaml")
				replaceInFile(t, path, "    - example", "    - ghost")
			},
			code: 1,
			want: `manifest.yaml Manifest/example-instance: spec.gaggles references "ghost", but no Gaggle/ghost definition was found`,
		},
		{
			name: "capability typo",
			mutate: func(t *testing.T, root string) {
				// Extends the existing capabilities: list rather than
				// appending a second one (#3643: a duplicate key is now a
				// hard error, so this can no longer rely on last-key-wins).
				path := filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "goober.yaml")
				replaceInFile(t, path, "  capabilities:\n    - repo:push", "  capabilities:\n    - repo:push\n    - github:prs:write")
			},
			code: 1,
			want: `Goober/coder: spec.capabilities contains unknown capability "github:prs:write"; did you mean "github:pr:write"?`,
		},
		{
			name: "missing instructions",
			mutate: func(t *testing.T, root string) {
				path := filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "instructions.md")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
			code: 1,
			want: `Goober/coder: spec.instructions file "instructions.md" was not found`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "foreign")
			if code, _, stderr := runArgs(t, "init", root); code != 0 {
				t.Fatalf("init: code=%d stderr=%q", code, stderr)
			}
			if tc.mutate != nil {
				tc.mutate(t, root)
			}
			code, stdout, stderr := runArgs(t, "validate", root)
			if code != tc.code {
				t.Fatalf("validate code=%d, want %d; stdout=%q stderr=%q", code, tc.code, stdout, stderr)
			}
			if !strings.Contains(stdout, tc.want) {
				t.Fatalf("validate stdout missing %q:\n%s", tc.want, stdout)
			}
		})
	}
}

func TestValidateGaggleRepositoriesMatchInstanceRepos(t *testing.T) {
	tests := []struct {
		name       string
		sourceTree bool
		noRepos    bool
		mutate     func(t *testing.T, path string)
		want       string
	}{
		{
			name: "instance project",
			mutate: func(t *testing.T, path string) {
				replaceInFile(t, path, "    name: your-repo", "    name: your-rep")
			},
			want: `spec.project repository your-org/your-rep matches no instance repos[] entry; did you mean "your-org/your-repo"?`,
		},
		{
			name:       "source tree additional repo",
			sourceTree: true,
			mutate: func(t *testing.T, path string) {
				replaceInFile(t, path, "  backlog:", `  additionalRepos:
    - provider: github
      owner: your-org
      name: your-rep
      connectionRef: repo-token
  backlog:`)
			},
			want: `spec.additionalRepos[0] repository your-org/your-rep matches no instance repos[] entry; did you mean "your-org/your-repo"?`,
		},
		{
			name:    "instance project without configured repos",
			noRepos: true,
			mutate:  func(t *testing.T, path string) {},
			want:    `spec.project repository your-org/your-repo matches no instance repos[] entry`,
		},
		{
			name:       "source tree additional repo without configured repos",
			sourceTree: true,
			noRepos:    true,
			mutate: func(t *testing.T, path string) {
				replaceInFile(t, path, "  backlog:", `  additionalRepos:
    - provider: github
      owner: extra-org
      name: extra-repo
      connectionRef: repo-token
  backlog:`)
			},
			want: `spec.additionalRepos[0] repository extra-org/extra-repo matches no instance repos[] entry`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "config")
			args := []string{"validate", root}
			gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
			instancePath := filepath.Join(root, "instance.yaml")
			if tc.sourceTree {
				if _, err := instance.SeedQuickstartConfigSource(root); err != nil {
					t.Fatal(err)
				}
				args = []string{"validate", "--source-tree", root}
				gagglePath = filepath.Join(root, "gaggles", "example", "gaggle.yaml")
				instancePath = filepath.Join(root, instance.GuidedSourceInstanceFile)
			} else if code, _, stderr := runArgs(t, "init", root); code != 0 {
				t.Fatalf("init: code=%d stderr=%q", code, stderr)
			}
			if tc.noRepos {
				replaceInFile(t, instancePath, `repos:
- name: your-repo
  owner: your-org
  provider: github
  token:
    env: GOOBERS_GITHUB_TOKEN`, "repos: []")
			}
			tc.mutate(t, gagglePath)

			code, stdout, stderr := runArgs(t, args...)
			if code != 1 || stderr != "" {
				t.Fatalf("validate code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
			}
			if !strings.Contains(stdout, tc.want) {
				t.Fatalf("validate stdout missing %q:\n%s", tc.want, stdout)
			}
		})
	}
}

func TestValidateReportsSingleRepoEmptyProjectFallback(t *testing.T) {
	for _, sourceTree := range []bool{false, true} {
		name := "instance"
		if sourceTree {
			name = "source tree"
		}
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "config")
			args := []string{"validate", root}
			gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
			if sourceTree {
				if _, err := instance.SeedQuickstartConfigSource(root); err != nil {
					t.Fatal(err)
				}
				args = []string{"validate", "--source-tree", root}
				gagglePath = filepath.Join(root, "gaggles", "example", "gaggle.yaml")
			} else if code, _, stderr := runArgs(t, "init", root); code != 0 {
				t.Fatalf("init: code=%d stderr=%q", code, stderr)
			}
			replaceInFile(t, gagglePath, "    owner: your-org", `    owner: ""`)
			replaceInFile(t, gagglePath, "    name: your-repo", `    name: ""`)

			code, stdout, stderr := runArgs(t, args...)
			if code != 1 || stderr != "" {
				t.Fatalf("validate code=%d, want 1 for the existing required-field errors; stdout=%q stderr=%q", code, stdout, stderr)
			}
			want := "INFO Gaggle/example: empty spec.project binds to instance repos[0] your-org/your-repo"
			if !strings.Contains(stdout, want) {
				t.Fatalf("validate stdout missing %q:\n%s", want, stdout)
			}

			jsonArgs := append([]string{"validate", "--json"}, args[1:]...)
			code, stdout, stderr = runArgs(t, jsonArgs...)
			if code != 1 || stderr != "" {
				t.Fatalf("validate --json code=%d, want 1 for the existing required-field errors; stdout=%q stderr=%q", code, stdout, stderr)
			}
			envelope := decodeDiagnosticsEnvelope(t, stdout)
			assertDiagnosticsSchema(t, stdout)
			if envelope.Counts.Infos != 1 {
				t.Fatalf("validate --json info count=%d, want 1; findings=%+v", envelope.Counts.Infos, envelope.Findings)
			}
			var fallback *diagnosticFinding
			for i := range envelope.Findings {
				if envelope.Findings[i].Code == "REPO003" {
					fallback = &envelope.Findings[i]
					break
				}
			}
			if fallback == nil {
				t.Fatalf("validate --json missing REPO003 fallback finding: %+v", envelope.Findings)
			}
			if fallback.Severity != diagnosticSeverityInfo || fallback.Path != "/spec/project" ||
				fallback.Message != "empty spec.project binds to instance repos[0] your-org/your-repo" {
				t.Fatalf("validate --json fallback finding = %+v", *fallback)
			}
		})
	}
}

func TestValidateAllowsRepositoryFreeScratchOnlyGaggle(t *testing.T) {
	withDemoNetworkNoneProbe(t, func(context.Context) error { return nil })
	root := filepath.Join(t.TempDir(), "demo")
	if code, _, stderr := runArgs(t, "init", "--demo", root); code != 0 {
		t.Fatalf("init --demo: code=%d stderr=%q", code, stderr)
	}

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 || stderr != "" {
		t.Fatalf("validate code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
}

// TestValidateGitHubAnnotations is #687's config-repo PR gate: each finding
// becomes a GitHub Actions ::error/::warning workflow command anchored to its
// file, written to stderr so it composes cleanly with --json (stdout stays a
// single parseable JSON document either way).
func TestValidateGitHubAnnotations(t *testing.T) {
	root := filepath.Join(t.TempDir(), "foreign")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code=%d stderr=%q", code, stderr)
	}
	path := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, path, "  gaggle: example", "  gaggle: ghost")

	t.Run("plain", func(t *testing.T) {
		code, _, stderr := runArgs(t, "validate", "--github-annotations", root)
		if code != 1 {
			t.Fatalf("validate --github-annotations code=%d, want 1; stderr=%q", code, stderr)
		}
		want := `::error file=config/gaggles/example/workflows/default-implement.yaml,title=REF007::spec.gaggle names "ghost", but no Gaggle/ghost definition was found`
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr missing annotation %q:\n%s", want, stderr)
		}
	})

	t.Run("composes with --json", func(t *testing.T) {
		code, stdout, stderr := runArgs(t, "validate", "--json", "--github-annotations", root)
		if code != 1 {
			t.Fatalf("code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
		}
		if !strings.Contains(stderr, "::error file=") {
			t.Fatalf("stderr missing an annotation:\n%s", stderr)
		}
		if strings.Contains(stdout, "::error") || strings.Contains(stdout, "::warning") {
			t.Fatalf("--json stdout must stay pure JSON, no workflow commands: %s", stdout)
		}
		var envelope diagnosticsEnvelope
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatalf("stdout is not valid JSON with annotations enabled: %v\n%s", err, stdout)
		}
	})

	t.Run("a valid config never emits an error annotation", func(t *testing.T) {
		cleanRoot := filepath.Join(t.TempDir(), "clean")
		if code, _, stderr := runArgs(t, "init", cleanRoot); code != 0 {
			t.Fatalf("init: code=%d stderr=%q", code, stderr)
		}
		code, _, stderr := runArgs(t, "validate", "--github-annotations", cleanRoot)
		if code != 0 {
			t.Fatalf("code=%d, want 0; stderr=%q", code, stderr)
		}
		if strings.Contains(stderr, "::error") {
			t.Fatalf("a valid (exit 0) config must never emit an ::error annotation: %s", stderr)
		}
	})
}

func TestValidateCheckRepos(t *testing.T) {
	root := filepath.Join(t.TempDir(), "foreign")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init: code=%d stderr=%q", code, stderr)
	}
	t.Setenv("GOOBERS_GITHUB_TOKEN", "test-token")

	original := targetRepositoryReachable
	t.Cleanup(func() { targetRepositoryReachable = original })
	originalSize := targetRepositorySize
	t.Cleanup(func() { targetRepositorySize = originalSize })
	stubRepositoryRealityChecks(t, []string{"goobers", "goobers:claimed"}, 1, 1)

	called := 0
	targetRepositoryReachable = func(_ context.Context, repo instance.RepoRef, token string, _ credentials.StoreResolver) error {
		called++
		if repo.Owner != "your-org" || repo.Name != "your-repo" {
			t.Errorf("repository = %s/%s, want your-org/your-repo", repo.Owner, repo.Name)
		}
		if token != "test-token" {
			t.Errorf("token = %q, want resolved test token", token)
		}
		return nil
	}
	sizeCalled := 0
	targetRepositorySize = func(context.Context, instance.RepoRef, string) (int64, error) {
		sizeCalled++
		return 100, nil
	}
	code, stdout, stderr := runArgs(t, "validate", "--check-repos", root)
	if code != 0 {
		t.Fatalf("validate --check-repos: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if called != 1 || !strings.Contains(stdout, "REPOSITORY repos[0] your-org/your-repo: reachable") {
		t.Fatalf("repository check calls=%d stdout=%q", called, stdout)
	}
	if sizeCalled != 1 {
		t.Fatalf("size check calls=%d, want 1", sizeCalled)
	}
	if strings.Contains(stdout, "checkout-size threshold") {
		t.Fatalf("did not expect oversized-repo warning for a small repo:\n%s", stdout)
	}

	targetRepositoryReachable = func(context.Context, instance.RepoRef, string, credentials.StoreResolver) error {
		return errors.New("repository not found or access denied for test-token")
	}
	code, stdout, stderr = runArgs(t, "validate", "--check-repos", root)
	if code != 1 {
		t.Fatalf("failed repository check code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"REPOSITORY repos[0] your-org/your-repo: unreachable: repository not found or access denied for [REDACTED]",
		"Check the owner/name, token source, repository access, and network connection.",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("failed repository check output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "test-token") {
		t.Fatalf("repository check output leaked the resolved token: %q", stdout)
	}
}

// TestValidateRejectsUnsupportedDSLVersion pins the §8.3 cutover (#3507): 1.4
// transitioned from deprecated (the strict-neutral DVL020 nudge #2700 tested)
// to UNSUPPORTED, so a 1.4 pin is now a hard DVL030 error naming the 1.4→2.0
// recovery edge — it fails plain validate, not just --strict. No DSL version
// is LevelDeprecated any more, so the CLI's DVL020 strict-exemption filter
// (cmd/goobers/validate.go) is unreachable via a real pin; the DVL020 emission
// it filters is still covered at the api/validate layer against a synthetic
// matrix (TestCheckWorkflowDSLVersionDeprecatedWarnsWithReplacement).
func TestValidateRejectsUnsupportedDSLVersion(t *testing.T) {
	root := initDeterministicDemo(t)
	instancePath := filepath.Join(root, "instance.yaml")
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, instancePath, "your-org", "acme")
	replaceInFile(t, instancePath, "your-repo", "widgets")
	for range 2 {
		replaceInFile(t, gagglePath, "your-org", "acme")
		replaceInFile(t, gagglePath, "your-repo", "widgets")
	}
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, workflowPath, `dslVersion: "2.0"`, `dslVersion: "1.4"`)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("validate code=%d, want 1 (1.4 is unsupported); stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		`dslVersion "1.4" is unsupported by this binary (replacement "2.0"); migrate with ` + "`goobers fix --to 2.0`",
		"config directory failed validation",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("validate output missing %q:\n%s", want, stdout)
		}
	}
}

func TestValidateStrictFailsOnWarnings(t *testing.T) {
	root := initDeterministicDemo(t)
	instancePath := filepath.Join(root, "instance.yaml")
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, instancePath, "your-org", "acme")
	replaceInFile(t, instancePath, "your-repo", "widgets")
	for range 2 {
		replaceInFile(t, gagglePath, "your-org", "acme")
		replaceInFile(t, gagglePath, "your-repo", "widgets")
	}
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, workflowPath, `        command: ["true"]`, "        command: [\"true\"]\n      expectedOutputs:\n        - artifact")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("advisory validate code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "expectedOutputs is declared but the stage has no inputs.resultFile") {
		t.Fatalf("advisory validate did not render warning:\n%s", stdout)
	}

	code, stdout, stderr = runArgs(t, "validate", "--strict", root)
	if code != 1 {
		t.Fatalf("strict validate code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"expectedOutputs is declared but the stage has no inputs.resultFile",
		"configuration has 1 warning(s); --strict treats warnings as errors",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("strict validate output missing %q:\n%s", want, stdout)
		}
	}
}

func TestValidateTemplatePlaceholders(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"WARNING PLACEHOLDER001 instance.yaml: contains unedited template marker(s) your-org, your-repo",
		"WARNING PLACEHOLDER001 config/gaggles/example/gaggle.yaml: contains unedited template marker(s) your-org, your-repo",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("validate output missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = runArgs(t, "validate", "--strict", root)
	if code != 1 {
		t.Fatalf("validate --strict code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Count(stdout, "ERROR PLACEHOLDER001") != 2 {
		t.Fatalf("strict validate did not promote both placeholder findings:\n%s", stdout)
	}

	code, stdout, stderr = runArgs(t, "validate", "--json", "--strict", root)
	if code != 1 || stderr != "" {
		t.Fatalf("validate --json --strict code=%d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	var envelope diagnosticsEnvelope
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Counts.Errors != 2 {
		t.Fatalf("strict diagnostics counts = %+v, want two errors", envelope.Counts)
	}
}

func TestValidateTemplatePlaceholdersClearAfterEditing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.InitQuickstart(root); err != nil {
		t.Fatal(err)
	}
	instancePath := filepath.Join(root, "instance.yaml")
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, instancePath, "your-org", "acme")
	replaceInFile(t, instancePath, "your-repo", "widgets")
	for range 2 {
		replaceInFile(t, gagglePath, "your-org", "acme")
		replaceInFile(t, gagglePath, "your-repo", "widgets")
	}

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate code=%d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, placeholderFindingCode) {
		t.Fatalf("edited quickstart still has placeholder findings:\n%s", stdout)
	}
}

func TestValidateTemplatePlaceholdersDoNotMatchEditedCoordinateSubstrings(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.InitDemo(root); err != nil {
		t.Fatal(err)
	}
	instancePath := filepath.Join(root, "instance.yaml")
	// InitDemo's scaffold already writes a "repos: null" line (Config.Repos
	// has no omitempty), so replace it in place rather than appending a
	// second repos: key (#3643: a duplicate key is now a hard error).
	replaceInFile(t, instancePath, "repos: null", `repos:
  - provider: github
    owner: your-organization
    name: your-repository
    token:
      env: GOOBERS_GITHUB_TOKEN`)

	code, stdout, stderr := runArgs(t, "validate", "--strict", root)
	if code != 0 {
		t.Fatalf("validate --strict code=%d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, placeholderFindingCode) {
		t.Fatalf("edited repository coordinates produced placeholder findings:\n%s", stdout)
	}
}

func TestValidateWarnsOnMissingSkillPackages(t *testing.T) {
	root := initDemo(t)
	// The starter scaffold now ships its scoped packages under
	// config/gaggles/example/skills (SKILL002 fix) rather than the
	// instance-level fallback; remove those to reproduce the missing-package
	// probe.
	if err := os.RemoveAll(filepath.Join(root, "config", "gaggles", "example", "skills")); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `WARNING SKILL002 gaggles/example/goobers/coder/goober.yaml Goober/coder: spec.skills declares "implement"`) {
		t.Fatalf("validate output omitted missing skill warning:\n%s", stdout)
	}

	for _, skill := range []string{"implement", "run-tests"} {
		if err := os.MkdirAll(filepath.Join(root, "skills", skill), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate with packages code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "SKILL002") {
		t.Fatalf("validate warned for present skill packages:\n%s", stdout)
	}
}

func TestValidateModelFallbackWarnsAndUsesAdvisoryExit(t *testing.T) {
	root := initDemo(t)
	gooberPath := filepath.Join(root, "config", "gaggles", "example", "goobers", "coder", "goober.yaml")
	replaceInFile(t, gooberPath, "  model: auto", "  model: retired-model")
	replaceInFile(t, gooberPath, "  harnessOptions: {}", "  harnessOptions:\n    fallback-to-default: true")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	want := `WARNING MODEL002 Goober/coder: requested model "retired-model" is unavailable; using the harness default`
	if !strings.Contains(stdout, want) {
		t.Fatalf("validate output missing %q:\n%s", want, stdout)
	}

	code, stdout, stderr = runArgs(t, "validate", "--json", root)
	if code != 0 || stderr != "" {
		t.Fatalf("validate --json code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	assertFindingSource(
		t,
		decodeDiagnosticsEnvelope(t, stdout),
		string(validate.WarningModelFallback),
		filepath.ToSlash(filepath.Join("config", "gaggles", "example", "goobers", "coder", "goober.yaml")),
		"/spec/harnessOptions/fallback-to-default",
	)

	code, stdout, stderr = runArgs(t, "validate", "--strict", root)
	if code != 1 {
		t.Fatalf("strict validate code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}

	replaceInFile(t, gooberPath, "    fallback-to-default: true", "    fallback-to-default: false")
	code, stdout, stderr = runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("invalid model validate code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `unknown model "retired-model"; valid models:`) {
		t.Fatalf("invalid model output omitted valid-model list:\n%s", stdout)
	}
}

func TestValidatePrintsDSLVersionSummary(t *testing.T) {
	root := initDeterministicDemo(t)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "DSLVERSION Workflow/default-implement: 2.0 (supported)") {
		t.Fatalf("validate output missing the DSL version summary line:\n%s", stdout)
	}
}

func TestValidateErrorsOnMissingDSLVersionPin(t *testing.T) {
	root := initDeterministicDemo(t)
	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, workflowPath, "dslVersion: \"2.0\"\n", "")

	code, stdout, stderr := runArgs(t, "validate", root)
	// The §8.3 cutover (#3507) dropped DSL 1.4, so a missing dslVersion is no
	// longer a warn-and-default-to-1.4 nudge — it is a hard error (DVL001)
	// naming the versions the author may pin. Validation now fails.
	if code != 1 {
		t.Fatalf("validate: code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"spec has no dslVersion pin; pin an explicit dslVersion (loadable: 2.0, 3.0) — the transitional default is gone now that DSL 1.4 is dropped",
		"config directory failed validation",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("validate output missing %q:\n%s", want, stdout)
		}
	}
}

func TestValidateRejectsUnmetProviderCapabilityRequirement(t *testing.T) {
	root := initDeterministicDemo(t)
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, gagglePath, "provider: github\n    owner: your-org", "provider: ado\n    project: your-project\n    owner: your-org")

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, workflowPath, "spec:\n  gaggle: example",
		"spec:\n  gaggle: example\n  requires:\n    capabilities:\n      - pr.review.threads")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("validate: code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"requires provider capability",
		"pr.review.threads",
		`"ado"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("validate output missing %q:\n%s", want, stdout)
		}
	}
}

// siblingSameRepoYAML declares a sibling targeting the exact repo
// initDeterministicDemo's example gaggle uses (github/your-org/your-repo).
const siblingSameRepoYAML = `  requireLabels:
    - area:frontend
  siblings:
    - project:
        provider: github
        owner: your-org
        name: your-repo
      label: Billing team
      requireLabels:
        - area:frontend
`

func TestValidateWarnsOnSiblingLabelOverlap(t *testing.T) {
	root := initDeterministicDemo(t)
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, gagglePath, "  isolation:\n    namespace: gaggle-example\n",
		"  isolation:\n    namespace: gaggle-example\n"+siblingSameRepoYAML)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d, want 0 (warning-only); stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"SIB001",
		`overlaps declared sibling "Billing team"`,
		"area:frontend",
		"your-org/your-repo",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("validate output missing %q:\n%s", want, stdout)
		}
	}
}

func TestValidateWarnsOnUnpartitionedGaggleWithSibling(t *testing.T) {
	root := initDeterministicDemo(t)
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, gagglePath, "  isolation:\n    namespace: gaggle-example\n",
		`  isolation:
    namespace: gaggle-example
  siblings:
    - project:
        provider: github
        owner: your-org
        name: your-repo
      label: Billing team
      requireLabels:
        - area:billing
`)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d, want 0 (warning-only); stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"SIB001",
		"spec.requireLabels is empty",
		`no label partition from declared sibling "Billing team"`,
		"your-org/your-repo",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("validate output missing %q:\n%s", want, stdout)
		}
	}
}

func TestValidateNoWarningOnDisjointSiblingLabels(t *testing.T) {
	root := initDeterministicDemo(t)
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, gagglePath, "  isolation:\n    namespace: gaggle-example\n",
		"  isolation:\n    namespace: gaggle-example\n"+
			`  requireLabels:
    - area:frontend
  siblings:
    - project:
        provider: github
        owner: your-org
        name: your-repo
      label: Billing team
      requireLabels:
        - area:billing
`)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "SIB001") {
		t.Fatalf("validate output unexpectedly warned on disjoint sibling labels:\n%s", stdout)
	}
}

func TestValidateNoWarningOnSiblingDifferentRepo(t *testing.T) {
	root := initDeterministicDemo(t)
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, gagglePath, "  isolation:\n    namespace: gaggle-example\n",
		"  isolation:\n    namespace: gaggle-example\n"+
			`  requireLabels:
    - area:frontend
  siblings:
    - project:
        provider: github
        owner: some-other-org
        name: unrelated-repo
      label: Unrelated team
      requireLabels:
        - area:frontend
`)

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "SIB001") {
		t.Fatalf("validate output unexpectedly warned on a sibling targeting a different repo:\n%s", stdout)
	}
}

func TestValidateWorkflowOverrideChangesSiblingOverlapScope(t *testing.T) {
	root := initDeterministicDemo(t)
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, gagglePath, "  isolation:\n    namespace: gaggle-example\n",
		"  isolation:\n    namespace: gaggle-example\n"+siblingSameRepoYAML)

	workflowPath := filepath.Join(root, "config", "gaggles", "example", "workflows", "default-implement.yaml")
	replaceInFile(t, workflowPath, "  start: local-ci\n  tasks:\n    - name: local-ci\n",
		"  start: claim-work\n  tasks:\n"+
			"    - name: claim-work\n"+
			"      type: deterministic\n"+
			"      goal: claim work\n"+
			"      run:\n"+
			"        command: [\"goobers\", \"backlog-query\"]\n"+
			"      capabilities: [\"github:issues:write\"]\n"+
			"      inputs:\n"+
			"        requireLabels: \"goobers:ready,area:special\"\n"+
			"      next: local-ci\n"+
			"    - name: local-ci\n")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("validate: code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	// The task's own requireLabels ("goobers:ready,area:special") fully
	// replaces the gaggle default ("area:frontend") for this workflow, so it
	// no longer overlaps the sibling's declared "area:frontend" — proving
	// the override, not the gaggle default, drove the comparison.
	if strings.Contains(stdout, "SIB001") {
		t.Fatalf("validate output unexpectedly warned despite the workflow's own requireLabels override:\n%s", stdout)
	}
}

func TestCheckTargetRepositoriesAllowsTokenlessADOAuth(t *testing.T) {
	original := targetRepositoryReachable
	t.Cleanup(func() { targetRepositoryReachable = original })

	targetRepositoryReachable = func(_ context.Context, repo instance.RepoRef, token string, _ credentials.StoreResolver) error {
		if repo.Provider != "ado" || repo.Project != "widgets" {
			t.Fatalf("repository = %#v", repo)
		}
		if token != "" {
			t.Fatalf("token = %q, want no materialized token", token)
		}
		return nil
	}
	var stdout strings.Builder
	ok := checkTargetRepositoriesAtFile([]instance.RepoRef{{
		Provider: "ado",
		Owner:    "acme",
		Project:  "widgets",
		Name:     "web",
		Auth:     &instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI},
	}}, nil, &stdout, "instance.yaml")
	if !ok || !strings.Contains(stdout.String(), "reachable") {
		t.Fatalf("checkTargetRepositories() = %v, output %q", ok, stdout.String())
	}
}

func TestCheckTargetRepositoriesWarnsOnOversizedGitHubRepo(t *testing.T) {
	t.Setenv("GITHUB_TOKEN_TEST_1547", "test-token")
	originalReachable := targetRepositoryReachable
	t.Cleanup(func() { targetRepositoryReachable = originalReachable })
	targetRepositoryReachable = func(context.Context, instance.RepoRef, string, credentials.StoreResolver) error {
		return nil
	}
	originalSize := targetRepositorySize
	t.Cleanup(func() { targetRepositorySize = originalSize })
	targetRepositorySize = func(context.Context, instance.RepoRef, string) (int64, error) {
		return 2 * oversizedRepoThresholdKB, nil
	}

	var stdout strings.Builder
	ok := checkTargetRepositoriesAtFile([]instance.RepoRef{{
		Provider: "github",
		Owner:    "acme",
		Name:     "monorepo",
		Token:    instance.TokenRef{Env: "GITHUB_TOKEN_TEST_1547"},
	}}, nil, &stdout, "instance.yaml")
	if !ok {
		t.Fatalf("checkTargetRepositories() = %v, want true (a size warning is advisory, not a failure)", ok)
	}
	for _, want := range []string{
		"REPOSITORY repos[0] acme/monorepo: WARNING: repository is 2048 MB, larger than the 1024 MB checkout-size threshold",
		"AdditionalRepos",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("checkTargetRepositories() output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestCheckTargetRepositoriesSizeCheckFailureIsNonFatal(t *testing.T) {
	t.Setenv("GITHUB_TOKEN_TEST_1547", "test-token")
	originalReachable := targetRepositoryReachable
	t.Cleanup(func() { targetRepositoryReachable = originalReachable })
	targetRepositoryReachable = func(context.Context, instance.RepoRef, string, credentials.StoreResolver) error {
		return nil
	}
	originalSize := targetRepositorySize
	t.Cleanup(func() { targetRepositorySize = originalSize })
	targetRepositorySize = func(context.Context, instance.RepoRef, string) (int64, error) {
		return 0, errors.New("rate limited")
	}

	var stdout strings.Builder
	ok := checkTargetRepositoriesAtFile([]instance.RepoRef{{
		Provider: "github",
		Owner:    "acme",
		Name:     "monorepo",
		Token:    instance.TokenRef{Env: "GITHUB_TOKEN_TEST_1547"},
	}}, nil, &stdout, "instance.yaml")
	if !ok {
		t.Fatalf("checkTargetRepositories() = %v, want true (size-check failure must not fail --check-repos)", ok)
	}
	if !strings.Contains(stdout.String(), "REPOSITORY repos[0] acme/monorepo: could not determine repository size: rate limited") {
		t.Fatalf("checkTargetRepositories() output missing size-check-failure diagnostic:\n%s", stdout.String())
	}
}

// TestValidateRejectsInsecureNonLoopbackOTLP reproduces #3333's live
// v0.2.0 Goobernetes cutover incident: an instance.yaml with
// telemetry.otlp.insecure: true against a non-loopback collector endpoint
// passed every check available at the time and only killed the daemon (and
// both workers) at boot with the runtime's "insecure mode is allowed only
// for localhost or a loopback IP" refusal. `goobers validate` must reach
// that same refusal — config-load parity — so this dies in CI, not at
// cutover, and the message must name both escape routes (a loopback sidecar
// collector, or a TLS endpoint) so the failure teaches the fix.
func TestValidateRejectsInsecureNonLoopbackOTLP(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	instancePath := filepath.Join(root, "instance.yaml")
	// Init's scaffold already writes a "telemetry: {}" line (TelemetryConfig
	// is a struct, so json/yaml omitempty cannot drop it), so replace it in
	// place rather than appending a second telemetry: key (#3643: a
	// duplicate key is now a hard error).
	replaceInFile(t, instancePath, "telemetry: {}", "telemetry:\n"+
		"  otlp:\n"+
		"    endpoint: goobers-collector.goobers-system:4317\n"+
		"    insecure: true")

	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 1 {
		t.Fatalf("validate code=%d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		`insecure mode is allowed only for localhost or a loopback IP`,
		`loopback sidecar collector`,
		`TLS collector`,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("validate stdout missing %q:\n%s", want, stdout)
		}
	}
}

func replaceInFile(t *testing.T, path, old, replacement string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(raw), old, replacement, 1)
	if updated == string(raw) {
		t.Fatalf("%s does not contain %q", path, old)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendToFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestValidateStrictDoesNotPromoteUnhonoredConnectionRef covers REF012's
// strict-neutrality (#3296). The finding says the local runtime honors no
// connectionRef at all — an author who needs the field for a cloud-tier
// deployment cannot silence it by editing their config — so promoting it
// under --strict would turn an existing green pipeline red on upgrade. It
// must still print.
func TestValidateStrictDoesNotPromoteUnhonoredConnectionRef(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instance")
	if _, err := instance.InitQuickstart(root); err != nil {
		t.Fatal(err)
	}
	instancePath := filepath.Join(root, "instance.yaml")
	gagglePath := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	replaceInFile(t, instancePath, "your-org", "acme")
	replaceInFile(t, instancePath, "your-repo", "widgets")
	for range 2 {
		replaceInFile(t, gagglePath, "your-org", "acme")
		replaceInFile(t, gagglePath, "your-repo", "widgets")
	}
	// The scaffolded gaggle no longer declares a connection, so the author's
	// declaration is added here — REF012 is about what an author writes.
	replaceInFile(t, gagglePath, "    branch: main", `    branch: main
    connectionRef: repo-token`)

	code, stdout, stderr := runArgs(t, "validate", "--strict", root)
	if code != 0 {
		t.Fatalf("validate --strict code=%d, stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "REF012") {
		t.Fatalf("strict validate did not report the unhonored connectionRef:\n%s", stdout)
	}
}
