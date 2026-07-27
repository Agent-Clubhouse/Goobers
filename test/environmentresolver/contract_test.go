package environmentresolver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestResolverScriptedFixtures(t *testing.T) {
	const (
		credentialLocator = "RESOLVER_FIXTURE_TOKEN"
		ambientSecret     = "TOP_SECRET_MUST_NOT_APPEAR"
	)
	t.Setenv(credentialLocator, ambientSecret)
	document := loadFixtureDocument(t)
	for _, scenario := range document.Scenarios {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			environment := materializeScenario(t, scenario)
			for _, run := range scenario.Runs {
				run := run
				t.Run(run.Name, func(t *testing.T) {
					environment.cli.calls = nil
					environment.provider.calls = nil
					report := resolveEnvironment(environment, resolverInputs{
						start:              fixturePath(environment.root, run.Start),
						instance:           fixturePath(environment.root, run.Instance),
						configSource:       fixturePath(environment.root, run.ConfigSource),
						binary:             fixturePath(environment.root, run.Binary),
						source:             fixturePath(environment.root, run.Source),
						instanceCandidates: expandFixturePaths(environment.root, run.InstanceCandidates),
					})
					assertFixtureReport(t, environment.root, run.Want, report)
					assertReadOnlyCalls(t, environment.cli.calls, environment.provider.calls)

					encoded, err := json.Marshal(report)
					if err != nil {
						t.Fatal(err)
					}
					for _, forbidden := range []string{credentialLocator, ambientSecret} {
						if strings.Contains(string(encoded), forbidden) {
							t.Fatalf("resolver report contains credential material %q: %s", forbidden, encoded)
						}
					}
				})
			}
		})
	}
}

func TestResolverFixturesCoverAcceptanceMatrix(t *testing.T) {
	document := loadFixtureDocument(t)
	runs := make(map[string]bool)
	scenarios := make(map[string]bool)
	for _, scenario := range document.Scenarios {
		scenarios[scenario.Name] = true
		for _, run := range scenario.Runs {
			runs[run.Name] = true
		}
	}
	for _, name := range []string{
		"config repository",
		"initialized instance",
		"target checkout",
		"Goobers source checkout",
		"unrelated parent",
		"manual manifest verification",
		"refuse toolkit and remote mismatch",
		"reject matching tag with conflicting commit",
		"reject tracked contract symlink",
		"reject ambiguous abbreviated commit",
	} {
		if !runs[name] {
			t.Errorf("resolver fixture suite is missing %q", name)
		}
	}
	for _, name := range []string{
		"starting directories and multiple targets",
		"matching remote release",
		"ambiguous instance",
		"intact toolkit without binary",
		"known version mismatch",
		"conflicting local release identities",
		"tracked symlink local source",
		"matching annotated remote release",
		"ambiguous abbreviated commit",
	} {
		if !scenarios[name] {
			t.Errorf("resolver fixture suite is missing scenario %q", name)
		}
	}
}

func TestResolverSkillMatchesFixtureContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "skills", "goobers-environment-resolver", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	skill := string(data)
	assertSubstringsInOrder(t, skill,
		"### 1. Record the environment locations",
		"version --json",
		"versions --json",
		"config show --json",
		"### 2. Establish an exact release identity",
		"### 3. Select and verify one contract source",
		"#### Matching local source checkout",
		"#### Matching installed toolkit",
		"#### Matching remote release ref",
		"### 4. Retain target-repository evidence",
		"## Required report",
	)
	for _, required := range []string{
		"config repository",
		"initialized instance",
		"Goobers source checkout",
		"target application",
		"unresolved",
		"github/<owner>/<name>",
		"entire sanitized provider key",
		"`dslVersions[]`",
		"`agent-kit check` has no JSON mode",
		"an exact tag match never overrides",
		"Resolve an abbreviated commit",
		"reject mode `120000`",
		"Never query or link to `main`",
		"`spec.additionalRepos`",
	} {
		if !strings.Contains(skill, required) {
			t.Errorf("resolver skill is missing fixture-backed directive %q", required)
		}
	}
}

func TestCommitMatchRequiresUniqueObject(t *testing.T) {
	first := "abc1000000000000000000000000000000000000"
	second := "abc1ffffffffffffffffffffffffffffffffffff"
	unique := "def4000000000000000000000000000000000000"
	objects := []string{first, second, unique}
	tests := []struct {
		name  string
		left  string
		right string
		set   []string
		want  bool
	}{
		{name: "full identity", left: first, right: first, want: true},
		{name: "unique abbreviation", left: unique, right: "def4", set: objects, want: true},
		{name: "ambiguous abbreviation", left: first, right: "abc1", set: objects},
		{name: "one character abbreviation", left: unique, right: "d", set: []string{unique}},
		{name: "abbreviation without object set", left: unique, right: "def4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commitsMatch(tt.left, tt.right, tt.set); got != tt.want {
				t.Fatalf("commitsMatch(%q, %q) = %t, want %t", tt.left, tt.right, got, tt.want)
			}
		})
	}
}

func TestRemoteReleaseUsesGitHubRefWorkflow(t *testing.T) {
	document := loadFixtureDocument(t)
	tests := []struct {
		scenario   string
		version    string
		commit     string
		annotated  bool
		fullCommit string
	}{
		{
			scenario:   "matching remote release",
			version:    "v2.0.0",
			commit:     "def4560000000000000000000000000000000000",
			fullCommit: "def4560000000000000000000000000000000000",
		},
		{
			scenario:   "matching annotated remote release",
			version:    "v2.1.0",
			commit:     "fedcba",
			annotated:  true,
			fullCommit: "fedcba0000000000000000000000000000000000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.scenario, func(t *testing.T) {
			environment := materializeScenario(t, fixtureScenarioNamed(t, document, tt.scenario))
			contract, ok := verifyRemoteRelease(environment.provider, binaryIdentity{
				Version: tt.version,
				Commit:  tt.commit,
			})
			if !ok {
				t.Fatal("matching remote release was unresolved")
			}
			if contract.Commit != tt.fullCommit {
				t.Fatalf("remote commit = %q, want %q", contract.Commit, tt.fullCommit)
			}
			if got := containsSubstring(environment.provider.calls, "release-tag "); got != tt.annotated {
				t.Fatalf("annotated tag peel call present = %t, want %t; calls = %v", got, tt.annotated, environment.provider.calls)
			}
			for _, path := range requiredContractPaths {
				call := "release-content " + tt.fullCommit + " " + path
				if !slices.Contains(environment.provider.calls, call) {
					t.Errorf("provider calls %v do not contain %q", environment.provider.calls, call)
				}
			}
		})
	}
}

func TestRemoteReleaseProviderFailuresRemainUnresolved(t *testing.T) {
	document := loadFixtureDocument(t)
	tests := []struct {
		name     string
		scenario string
		identity binaryIdentity
		breakIt  func(*fakeProvider)
	}{
		{
			name:     "annotated tag peel",
			scenario: "matching annotated remote release",
			identity: binaryIdentity{Version: "v2.1.0", Commit: "fedcba"},
			breakIt: func(provider *fakeProvider) {
				provider.outputs.ReleaseTags = nil
			},
		},
		{
			name:     "release tree",
			scenario: "matching remote release",
			identity: binaryIdentity{Version: "v2.0.0", Commit: "def4560000000000000000000000000000000000"},
			breakIt: func(provider *fakeProvider) {
				provider.outputs.ReleaseTree = ""
			},
		},
		{
			name:     "required content",
			scenario: "matching remote release",
			identity: binaryIdentity{Version: "v2.0.0", Commit: "def4560000000000000000000000000000000000"},
			breakIt: func(provider *fakeProvider) {
				delete(provider.outputs.ReleaseContents, requiredContractPaths[0])
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			environment := materializeScenario(t, fixtureScenarioNamed(t, document, tt.scenario))
			tt.breakIt(environment.provider)
			if _, ok := verifyRemoteRelease(environment.provider, tt.identity); ok {
				t.Fatal("remote provider failure selected a contract")
			}
		})
	}
}

func fixtureScenarioNamed(t *testing.T, document fixtureDocument, name string) fixtureScenario {
	t.Helper()
	for _, scenario := range document.Scenarios {
		if scenario.Name == name {
			return scenario
		}
	}
	t.Fatalf("fixture scenario %q not found", name)
	return fixtureScenario{}
}

func TestAgentKitCheckFixtureUsesTextContract(t *testing.T) {
	output := []byte("state: current\n" +
		"bundle version: 1\n" +
		"source binary version: v1.2.3\n" +
		"source binary commit: abc123\n" +
		"installed bundle version: 1\n" +
		"installed source version: v1.2.3\n" +
		"installed source commit: abc123\n" +
		"update available: no\n" +
		"modified owned files: none\n" +
		"missing owned files: none\n")
	check, ok := parseAgentKitCheck(output)
	if !ok {
		t.Fatal("real agent-kit check text was not parsed")
	}
	if check.State != "current" || check.SourceBinaryVersion != "v1.2.3" ||
		check.InstalledSourceCommit != "abc123" {
		t.Fatalf("parsed agent-kit check = %+v", check)
	}
	if _, ok := parseAgentKitCheck([]byte(`{"state":"current"}`)); ok {
		t.Fatal("obsolete JSON agent-kit check shape was accepted")
	}
}

func TestRepositoryIdentityFromRemoteURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
		ok   bool
	}{
		{name: "GitHub HTTPS", url: "https://token@github.com/Acme/Web.git", want: "github/acme/web", ok: true},
		{name: "GitHub SSH", url: "git@github.com:Acme/Web.git", want: "github/acme/web", ok: true},
		{name: "ADO HTTPS", url: "https://dev.azure.com/Acme/Platform/_git/Web", want: "ado/acme/platform/web", ok: true},
		{name: "ADO SSH", url: "git@ssh.dev.azure.com:v3/Acme/Platform/Web", want: "ado/acme/platform/web", ok: true},
		{name: "unknown provider", url: "https://example.invalid/acme/web.git", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := repositoryIdentityFromRemoteURL(tt.url)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("repository identity = %q, %t; want %q, %t", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestRepositoryIdentityRequiresCompleteProviderKey(t *testing.T) {
	if got := repositoryIdentity(configRepo{Provider: "github", Owner: "acme", Name: "web"}); got != "github/acme/web" {
		t.Fatalf("GitHub identity = %q", got)
	}
	if got := repositoryIdentity(configRepo{Provider: "ado", Owner: "acme", Name: "web"}); got != "" {
		t.Fatalf("incomplete ADO identity = %q, want unresolved", got)
	}
	if got := repositoryIdentity(configRepo{Provider: "ado", Owner: "acme", Project: "platform", Name: "web"}); got != "ado/acme/platform/web" {
		t.Fatalf("ADO identity = %q", got)
	}
}

func assertFixtureReport(t *testing.T, root string, want fixtureWant, got resolverReport) {
	t.Helper()
	if got.CurrentRole != want.CurrentRole {
		t.Errorf("current role = %q, want %q", got.CurrentRole, want.CurrentRole)
	}
	if got.Executable.Provenance != want.BinaryProvenance {
		t.Errorf("binary provenance = %q, want %q", got.Executable.Provenance, want.BinaryProvenance)
	}
	if got.Contract.Kind != want.ContractKind {
		t.Errorf("contract kind = %q, want %q; diagnostics = %v", got.Contract.Kind, want.ContractKind, got.Diagnostics)
	}
	assertPathField(t, "instance", root, want.Instance, got.Instance)
	assertPathField(t, "config source", root, want.ConfigSource, got.ConfigSource)
	if got.Executable.Path != "" {
		if got.BinaryVersion == "" || got.BinaryCommit == "" {
			t.Errorf("selected binary has incomplete identity: version=%q commit=%q", got.BinaryVersion, got.BinaryCommit)
		}
		if len(got.DSLVersions) == 0 {
			t.Error("selected binary has no reported DSL support")
		}
	}
	if got.Contract.Kind != "unresolved" && len(got.Contract.Locations) != len(requiredContractPaths) {
		t.Errorf("contract locations = %d, want %d", len(got.Contract.Locations), len(requiredContractPaths))
	}

	gotTargets := make([]string, 0, len(got.Targets))
	for _, target := range got.Targets {
		gotTargets = append(gotTargets, target.Identity)
		if target.Access == "unresolved" {
			t.Errorf("target %s is unresolved: %s", target.Identity, target.Unresolved)
		}
		if len(target.Guidance) == 0 {
			t.Errorf("target %s has no README or agent guidance evidence", target.Identity)
		}
		if len(target.BuildOrCI) == 0 {
			t.Errorf("target %s has no build or CI evidence", target.Identity)
		}
	}
	if !slices.Equal(gotTargets, want.Targets) {
		t.Errorf("targets = %v, want %v", gotTargets, want.Targets)
	}
	for _, diagnostic := range want.DiagnosticsContain {
		if !containsSubstring(got.Diagnostics, diagnostic) {
			t.Errorf("diagnostics %v do not contain %q", got.Diagnostics, diagnostic)
		}
	}
}

func assertPathField(t *testing.T, name, root, want, got string) {
	t.Helper()
	want = strings.ReplaceAll(want, "$ROOT", filepath.ToSlash(root))
	if want != "" {
		want = filepath.Clean(filepath.FromSlash(want))
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func assertReadOnlyCalls(t *testing.T, cliCalls, providerCalls []string) {
	t.Helper()
	for _, call := range cliCalls {
		if !containsSubstring([]string{call},
			"version --json") &&
			!containsSubstring([]string{call}, "versions --json") &&
			!containsSubstring([]string{call}, "config show --json") &&
			!containsSubstring([]string{call}, "agent-kit check") {
			t.Errorf("fixture invoked non-read-only CLI command %q", call)
		}
	}
	for _, call := range providerCalls {
		if !strings.HasPrefix(call, "repository ") &&
			!strings.HasPrefix(call, "release-ref ") &&
			!strings.HasPrefix(call, "release-tag ") &&
			!strings.HasPrefix(call, "release-commit ") &&
			!strings.HasPrefix(call, "release-tree ") &&
			!strings.HasPrefix(call, "release-content ") &&
			!strings.HasPrefix(call, "target ") {
			t.Errorf("fixture invoked non-read-only provider operation %q", call)
		}
	}
}

func assertSubstringsInOrder(t *testing.T, text string, required ...string) {
	t.Helper()
	offset := 0
	for _, fragment := range required {
		index := strings.Index(text[offset:], fragment)
		if index < 0 {
			t.Fatalf("text does not contain %q after byte %d", fragment, offset)
		}
		offset += index + len(fragment)
	}
}

func expandFixturePaths(root string, paths []string) []string {
	expanded := make([]string, len(paths))
	for i, path := range paths {
		expanded[i] = fixturePath(root, path)
	}
	return expanded
}

func containsSubstring(values []string, substring string) bool {
	for _, value := range values {
		if strings.Contains(value, substring) {
			return true
		}
	}
	return false
}
