package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// connectTestInstance scaffolds a template instance in an isolated temp dir
// and returns its root. template is "starter" (bare init) or "quickstart".
// The default token env is cleared so an ambient developer token can never
// switch a hermetic test onto the live repository-check path.
func connectTestInstance(t *testing.T, template string) string {
	t.Helper()
	t.Setenv(connectDefaultTokenEnv, "")
	root := filepath.Join(onboardingTestTempDir(t), "instance")
	var err error
	switch template {
	case "starter":
		_, err = instance.Init(root)
	case "quickstart":
		_, err = instance.InitQuickstart(root)
	default:
		t.Fatalf("unknown template %q", template)
	}
	if err != nil {
		t.Fatalf("scaffold %s instance: %v", template, err)
	}
	return root
}

// stubConnectSeeder installs a fake provider seeder and clears the network
// reachability seams so no connect test ever leaves the process.
func stubConnectSeeder(t *testing.T) *fakeOnboardingIssueSeeder {
	t.Helper()
	seeder := &fakeOnboardingIssueSeeder{labels: map[string]bool{}}
	previousSeeder := newOnboardingIssueSeeder
	newOnboardingIssueSeeder = func(string) onboardingIssueSeeder { return seeder }
	t.Cleanup(func() { newOnboardingIssueSeeder = previousSeeder })
	stubConnectReachability(t, nil)
	return seeder
}

func stubConnectReachability(t *testing.T, err error) {
	t.Helper()
	previousReachable := targetRepositoryReachable
	targetRepositoryReachable = func(context.Context, instance.RepoRef, string, credentials.StoreResolver) error {
		return err
	}
	t.Cleanup(func() { targetRepositoryReachable = previousReachable })
	previousSize := targetRepositorySize
	targetRepositorySize = func(context.Context, instance.RepoRef, string) (int64, error) {
		return 1, nil
	}
	t.Cleanup(func() { targetRepositorySize = previousSize })
}

func connectEnvelope(t *testing.T, stdout string) onboardingActionResult {
	t.Helper()
	var result onboardingActionResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode connect envelope %q: %v", stdout, err)
	}
	return result
}

func TestConnectReplacesPlaceholdersStarter(t *testing.T) {
	root := connectTestInstance(t, "starter")
	code, stdout, stderr := runArgs(t, "connect", "acme/web", "--json", root)
	if code != 0 {
		t.Fatalf("connect: code=%d stderr=%q", code, stderr)
	}
	result := connectEnvelope(t, stdout)
	if result.Action != connectAction || result.Version != onboardingActionVersion {
		t.Fatalf("envelope identity = %q v%d", result.Action, result.Version)
	}
	if !slices.Contains(result.Updated, "instance.yaml") ||
		!slices.Contains(result.Updated, "config/gaggles/example/gaggle.yaml") {
		t.Fatalf("updated = %v", result.Updated)
	}
	if !strings.Contains(result.NextCommand, "goobers validate --check-harness --check-repos") {
		t.Fatalf("nextCommand = %q", result.NextCommand)
	}

	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Owner != "acme" || cfg.Repos[0].Name != "web" ||
		cfg.Repos[0].Provider != "github" || cfg.Repos[0].Token.Env != connectDefaultTokenEnv {
		t.Fatalf("repos = %+v", cfg.Repos)
	}

	gaggleData, err := os.ReadFile(filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(gaggleData)
	if strings.Contains(text, "your-org") || strings.Contains(text, "your-repo") {
		t.Fatalf("gaggle.yaml still carries placeholders:\n%s", text)
	}
	if !strings.Contains(text, "owner: acme") || !strings.Contains(text, "name: web") ||
		!strings.Contains(text, "project: acme/web") {
		t.Fatalf("gaggle.yaml not rewritten:\n%s", text)
	}
	// Comment-preserving surgery: the template's guidance comments survive.
	if !strings.Contains(text, "Point this at your own repo") ||
		!strings.Contains(text, "GAG-004") {
		t.Fatalf("gaggle.yaml comments did not survive the rewrite:\n%s", text)
	}

	// The rewritten instance must pass full validation on its own.
	var out strings.Builder
	if code := runValidate([]string{root}, &out, &out); code != 0 {
		t.Fatalf("validate after connect: code=%d output=%s", code, out.String())
	}
}

func TestConnectReplacesPlaceholdersQuickstart(t *testing.T) {
	root := connectTestInstance(t, "quickstart")
	code, stdout, stderr := runArgs(t, "connect", "acme/web", "--json", root)
	if code != 0 {
		t.Fatalf("connect: code=%d stderr=%q", code, stderr)
	}
	result := connectEnvelope(t, stdout)
	if !slices.Contains(result.Updated, "instance.yaml") ||
		!slices.Contains(result.Updated, "config/gaggles/example/gaggle.yaml") {
		t.Fatalf("updated = %v", result.Updated)
	}
	gaggleData, err := os.ReadFile(filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(gaggleData)
	if strings.Contains(text, "your-org/your-repo") ||
		!strings.Contains(text, "project: acme/web") ||
		!strings.Contains(text, "owner: acme") {
		t.Fatalf("quickstart gaggle.yaml not rewritten:\n%s", text)
	}
	var out strings.Builder
	if code := runValidate([]string{root}, &out, &out); code != 0 {
		t.Fatalf("validate after connect: code=%d output=%s", code, out.String())
	}
}

func TestConnectIdempotentRerun(t *testing.T) {
	root := connectTestInstance(t, "quickstart")
	if code, _, stderr := runArgs(t, "connect", "acme/web", root); code != 0 {
		t.Fatalf("first connect: stderr=%q", stderr)
	}
	code, stdout, stderr := runArgs(t, "connect", "acme/web", "--json", root)
	if code != 0 {
		t.Fatalf("second connect: code=%d stderr=%q", code, stderr)
	}
	result := connectEnvelope(t, stdout)
	if len(result.Updated) != 0 {
		t.Fatalf("second connect updated = %v, want none", result.Updated)
	}
	if !slices.Contains(result.Skipped, "instance.yaml") ||
		!slices.Contains(result.Skipped, "config/gaggles/example/gaggle.yaml") {
		t.Fatalf("second connect skipped = %v", result.Skipped)
	}
}

func TestConnectRefusesWorkflowSourceInstance(t *testing.T) {
	root := connectTestInstance(t, "quickstart")
	configFile := instance.NewLayout(root).ConfigFile()
	cfg, err := instance.LoadConfig(configFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.WorkflowSource = &instance.WorkflowSource{
		Kind: instance.WorkflowSourceKindLocalDir,
		Path: filepath.Join(root, "config"),
	}
	if err := instance.WriteConfig(configFile, cfg); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runArgs(t, "connect", "acme/web", root)
	if code != 1 {
		t.Fatalf("connect code = %d, want 1; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "materializes config from a source tree") ||
		!strings.Contains(stderr, "goobers config materialize") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestConnectRefusesNonPlaceholderWithoutReplace(t *testing.T) {
	root := connectTestInstance(t, "quickstart")
	if code, _, stderr := runArgs(t, "connect", "acme/web", root); code != 0 {
		t.Fatalf("first connect: stderr=%q", stderr)
	}
	code, _, stderr := runArgs(t, "connect", "other/repo", root)
	if code != 1 {
		t.Fatalf("connect code = %d, want 1; stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "no placeholder repository entry found") ||
		!strings.Contains(stderr, "repos[0] acme/web") ||
		!strings.Contains(stderr, "--replace") {
		t.Fatalf("stderr = %q", stderr)
	}

	replaced, stdout, stderr := runArgs(t, "connect", "other/repo", "--replace", "--json", root)
	if replaced != 0 {
		t.Fatalf("connect --replace: code=%d stderr=%q", replaced, stderr)
	}
	result := connectEnvelope(t, stdout)
	if !slices.Contains(result.Updated, "instance.yaml") ||
		!slices.Contains(result.Updated, "config/gaggles/example/gaggle.yaml") {
		t.Fatalf("--replace updated = %v", result.Updated)
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repos[0].Owner != "other" || cfg.Repos[0].Name != "repo" {
		t.Fatalf("repos[0] = %+v", cfg.Repos[0])
	}
	gaggleData, err := os.ReadFile(filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gaggleData), "project: other/repo") {
		t.Fatalf("gaggle.yaml not replaced:\n%s", gaggleData)
	}
}

func TestConnectRejectsPastedTokenValue(t *testing.T) {
	root := connectTestInstance(t, "quickstart")
	for _, pasted := range []string{"ghp_abcdef0123456789", "github_pat_11ABCDEF"} {
		code, _, stderr := runArgs(t, "connect", "acme/web", "--token-env", pasted, root)
		if code != 2 {
			t.Fatalf("connect --token-env %q code = %d, want 2", pasted, code)
		}
		if !strings.Contains(stderr, "do not provide a token value") {
			t.Fatalf("stderr = %q", stderr)
		}
	}
}

func TestConnectRejectsNonRepoPositional(t *testing.T) {
	root := connectTestInstance(t, "quickstart")
	for _, bad := range []string{"acme", "https://gitlab.com/acme/web", "github.com/acme/web"} {
		code, _, stderr := runArgs(t, "connect", bad, root)
		if code != 2 {
			t.Fatalf("connect %q code = %d, want 2; stderr=%q", bad, code, stderr)
		}
		if !strings.Contains(stderr, "GitHub is the only supported provider in v1") {
			t.Fatalf("connect %q stderr = %q", bad, stderr)
		}
	}
}

// TestConnectRefusesAzureDevOpsIdentity reproduces cold-start ado #7 attempt 1:
// the honest three-part ADO identity used to get a bare "GitHub is the only
// supported provider in v1" refusal that named no way forward. Every ADO
// spelling now gets the exact instance.yaml block to write by hand, and
// nothing on disk is touched.
func TestConnectRefusesAzureDevOpsIdentity(t *testing.T) {
	root := connectTestInstance(t, "quickstart")
	configFile := instance.NewLayout(root).ConfigFile()
	before, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}

	for _, identity := range []string{
		"contoso/example-project/example-repo",
		"https://dev.azure.com/contoso/example-project/_git/example-repo",
		"https://contoso.visualstudio.com/example-project/_git/example-repo",
		"git@ssh.dev.azure.com:v3/contoso/example-project/example-repo",
	} {
		code, _, stderr := runArgs(t, "connect", identity, "--token-env", "GOOBERS_ADO_TOKEN", root)
		if code != 2 {
			t.Fatalf("connect %q code = %d, want 2; stderr=%q", identity, code, stderr)
		}
		for _, want := range []string{
			connectADOIdentityCode,
			"Azure DevOps organization/project/repository identity",
			"provider: ado",
			"owner: contoso",
			"project: example-project",
			"name: example-repo",
			"env: GOOBERS_ADO_TOKEN",
			"spec.backlog.project",
			"docs/guides/ado-authentication.md",
			"reference-workflows/instance.yaml.example",
		} {
			if !strings.Contains(stderr, want) {
				t.Errorf("connect %q stderr lacks %q:\n%s", identity, want, stderr)
			}
		}
	}

	after, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("refused connect rewrote instance.yaml:\n%s", after)
	}
}

// TestConnectADOIdentityRecognition pins both directions of the guard: every
// Azure DevOps spelling resolves to all three coordinates, and nothing that is
// (or could be) a GitHub identity is ever claimed as ADO.
func TestConnectADOIdentityRecognition(t *testing.T) {
	want := connectADORepo{Organization: "contoso", Project: "example-project", Repository: "example-repo"}
	for _, value := range []string{
		"contoso/example-project/example-repo",
		"contoso/example-project/example-repo.git",
		"  contoso/example-project/example-repo  ",
		"https://dev.azure.com/contoso/example-project/_git/example-repo",
		"https://dev.azure.com/contoso/example-project/_git/example-repo.git",
		"https://contoso.visualstudio.com/example-project/_git/example-repo",
		"https://contoso.visualstudio.com/DefaultCollection/example-project/_git/example-repo",
		"git@ssh.dev.azure.com:v3/contoso/example-project/example-repo",
	} {
		got, ok := connectADOIdentity(value)
		if !ok || got != want {
			t.Errorf("connectADOIdentity(%q) = %+v, %v; want %+v, true", value, got, ok, want)
		}

		if got, ok := connectADOIdentity("https://dev.azure.com/contoso/Example%20Project/_git/example-repo"); !ok ||
			got != (connectADORepo{Organization: "contoso", Project: "Example Project", Repository: "example-repo"}) {
			t.Errorf("escaped project form = %+v, %v", got, ok)
		}
	}

	// The short dev.azure.com form ADO emits when project and repository share
	// a name.
	if got, ok := connectADOIdentity("https://dev.azure.com/contoso/_git/example-repo"); !ok ||
		got != (connectADORepo{Organization: "contoso", Project: "example-repo", Repository: "example-repo"}) {
		t.Errorf("short dev.azure.com form = %+v, %v", got, ok)
	}

	for _, value := range []string{
		"",
		"acme",
		"acme/web",
		"https://github.com/acme/web",
		"git@github.com:acme/web.git",
		"github.com/acme/web",
		"https://gitlab.com/acme/group/web",
		"https://gitea.example.com/acme/web",
		"acme//web",
		"acme/web/extra/more",
	} {
		if got, ok := connectADOIdentity(value); ok {
			t.Errorf("connectADOIdentity(%q) = %+v, true; want false", value, got)
		}
	}
}

// TestConnectADORefusalNeverEchoesPastedToken keeps a pasted secret out of the
// diagnostic: the refusal prints a token VARIABLE name, and a value that is
// not a legal variable name falls back to the ADO hint.
func TestConnectADORefusalNeverEchoesPastedToken(t *testing.T) {
	root := connectTestInstance(t, "quickstart")
	code, _, stderr := runArgs(t, "connect", "contoso/example-project/example-repo",
		"--token-env", "ghp_abcdef0123456789", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if strings.Contains(stderr, "ghp_abcdef0123456789") {
		t.Fatalf("refusal echoed the pasted token: %q", stderr)
	}
	if !strings.Contains(stderr, "env: "+connectADOTokenEnvHint) {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestConnectTwoPartADOGuessLeavesNothingOnDisk reproduces cold-start ado #7
// attempt 2, the sharpest onboarding bug found: the two-part guess at an ADO
// repository parses as a GitHub owner/name, and connect used to rewrite
// instance.yaml and the gaggle to provider: github BEFORE discovering that
// github.com does not have that repository. The preflight now runs first, so a
// failed connect leaves the template exactly as it was.
func TestConnectTwoPartADOGuessLeavesNothingOnDisk(t *testing.T) {
	const tokenEnv = "GOOBERS_ADO_TOKEN"
	t.Setenv(tokenEnv, "ado-pat")
	stubConnectReachability(t, errors.New("exit status 128: fatal: unable to get password from user"))

	root := connectTestInstance(t, "quickstart")
	configFile := instance.NewLayout(root).ConfigFile()
	gaggleFile := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	beforeConfig, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	beforeGaggle, err := os.ReadFile(gaggleFile)
	if err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runArgs(t, "connect", "contoso/example-repo", "--token-env", tokenEnv, root)
	if code != 1 {
		t.Fatalf("connect code = %d, want 1; stderr=%q", code, stderr)
	}
	for _, want := range []string{
		connectUnreachableCode,
		"contoso/example-repo is not reachable with the credential named by " + tokenEnv,
		"nothing was written",
		"docs/guides/ado-authentication.md",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}

	afterConfig, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterConfig) != string(beforeConfig) {
		t.Fatalf("failed connect rewrote instance.yaml:\n%s", afterConfig)
	}
	afterGaggle, err := os.ReadFile(gaggleFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterGaggle) != string(beforeGaggle) {
		t.Fatalf("failed connect rewrote gaggle.yaml:\n%s", afterGaggle)
	}
	// The placeholder survived, so the honest next step still works.
	cfg, err := instance.LoadConfig(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repos[0].Owner != connectPlaceholderOwner || cfg.Repos[0].Name != connectPlaceholderName {
		t.Fatalf("repos[0] = %+v, want the untouched placeholder", cfg.Repos[0])
	}
}

// TestConnectReachableTwoPartRepoStillWrites is the negative case for the
// preflight reordering: a reachable GitHub repository connects exactly as
// before.
func TestConnectReachableTwoPartRepoStillWrites(t *testing.T) {
	const tokenEnv = "GOOBERS_CONNECT_TEST_TOKEN"
	t.Setenv(tokenEnv, "test-token")
	stubConnectSeeder(t)

	root := connectTestInstance(t, "quickstart")
	code, stdout, stderr := runArgs(t, "connect", "acme/web", "--token-env", tokenEnv, "--json", root)
	if code != 0 {
		t.Fatalf("connect: code=%d stderr=%q", code, stderr)
	}
	result := connectEnvelope(t, stdout)
	if !slices.Contains(result.Updated, "instance.yaml") {
		t.Fatalf("updated = %v", result.Updated)
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repos[0].Owner != "acme" || cfg.Repos[0].Name != "web" {
		t.Fatalf("repos[0] = %+v", cfg.Repos[0])
	}
}

// TestConnectRefusesForeignProviderEntry covers the other ordering of cold-start
// ado #7: an operator who hand-wrote the ADO repos[] entry first (ado TWEAK 6)
// and then reached for connect. Rewriting that entry would drop its
// project/auth block, so connect refuses — with and without --replace, which
// the generic no-placeholder message used to recommend.
func TestConnectRefusesForeignProviderEntry(t *testing.T) {
	root := connectTestInstance(t, "quickstart")
	configFile := instance.NewLayout(root).ConfigFile()
	cfg, err := instance.LoadConfig(configFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos = []instance.RepoRef{{
		Provider: "ado",
		Owner:    "contoso",
		Project:  "example-project",
		Name:     "example-repo",
		Auth:     &instance.RepoAuthConfig{Kind: instance.ADOAuthPAT},
		Token:    instance.TokenRef{Env: "GOOBERS_ADO_TOKEN"},
	}}
	if err := instance.WriteConfig(configFile, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"connect", "contoso/example-repo", root},
		{"connect", "contoso/example-repo", "--replace", root},
	} {
		code, _, stderr := runArgs(t, args...)
		if code != 1 {
			t.Fatalf("%v code = %d, want 1; stderr=%q", args, code, stderr)
		}
		for _, want := range []string{
			connectForeignProviderCode,
			`repos[0] declares provider "ado"`,
			"contoso/example-project/example-repo",
			"provider: github entries only",
			"docs/guides/ado-authentication.md",
		} {
			if !strings.Contains(stderr, want) {
				t.Errorf("%v stderr lacks %q:\n%s", args, want, stderr)
			}
		}
		if strings.Contains(stderr, "re-run with --replace") {
			t.Errorf("%v invited --replace over a non-GitHub entry: %q", args, stderr)
		}
	}

	after, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("refused connect rewrote instance.yaml:\n%s", after)
	}
}

func TestConnectRequiresInstanceRoot(t *testing.T) {
	root := filepath.Join(onboardingTestTempDir(t), "empty")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runArgs(t, "connect", "acme/web", root)
	if code != 2 || !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestConnectSeedDerivesQuickstartSelectorLabels(t *testing.T) {
	const tokenEnv = "GOOBERS_CONNECT_TEST_TOKEN"
	t.Setenv(tokenEnv, "test-token")
	seeder := stubConnectSeeder(t)
	root := connectTestInstance(t, "quickstart")

	code, stdout, stderr := runArgs(t, "connect", "acme/web", "--token-env", tokenEnv, "--seed", "--json", root)
	if code != 0 {
		t.Fatalf("connect --seed: code=%d stderr=%q", code, stderr)
	}
	result := connectEnvelope(t, stdout)
	for _, want := range []string{"label:goobers:approved", "label:goobers:ready", "issue:" + connectSeedIssueID} {
		if !slices.Contains(result.Created, want) {
			t.Errorf("created lacks %q: %v", want, result.Created)
		}
	}
	if len(seeder.createRequests) != 1 {
		t.Fatalf("created issues = %d, want 1", len(seeder.createRequests))
	}
	request := seeder.createRequests[0]
	if request.Repository.Owner != "acme" || request.Repository.Name != "web" {
		t.Fatalf("issue repository = %+v", request.Repository)
	}
	if request.Title != connectSeedIssueTitle {
		t.Fatalf("issue title = %q", request.Title)
	}
	wantLabels := []string{"goobers:approved", "goobers:ready"}
	if !slices.Equal(request.Labels, wantLabels) {
		t.Fatalf("issue labels = %v, want %v", request.Labels, wantLabels)
	}
	if !strings.Contains(request.RunID, "onboarding/connect/") {
		t.Fatalf("issue run-id = %q", request.RunID)
	}
	// Both validation and the scoped repo check passed, so the honest next
	// rung is a real run.
	if !strings.Contains(result.NextCommand, "goobers run quickstart") {
		t.Fatalf("nextCommand = %q", result.NextCommand)
	}
}

func TestConnectSeedDerivesStarterSelectorLabels(t *testing.T) {
	const tokenEnv = "GOOBERS_CONNECT_TEST_TOKEN"
	t.Setenv(tokenEnv, "test-token")
	seeder := stubConnectSeeder(t)
	root := connectTestInstance(t, "starter")

	code, stdout, stderr := runArgs(t, "connect", "acme/web", "--token-env", tokenEnv, "--seed", "--json", root)
	if code != 0 {
		t.Fatalf("connect --seed: code=%d stderr=%q", code, stderr)
	}
	result := connectEnvelope(t, stdout)
	if !slices.Contains(result.Created, "label:goobers") ||
		!slices.Contains(result.Created, "issue:"+connectSeedIssueID) {
		t.Fatalf("created = %v", result.Created)
	}
	if len(seeder.createRequests) != 1 {
		t.Fatalf("created issues = %d, want 1", len(seeder.createRequests))
	}
	if !slices.Equal(seeder.createRequests[0].Labels, []string{"goobers"}) {
		t.Fatalf("issue labels = %v, want [goobers]", seeder.createRequests[0].Labels)
	}
	if !strings.Contains(result.NextCommand, "goobers run default-implement") {
		t.Fatalf("nextCommand = %q", result.NextCommand)
	}
}

func TestConnectSeedIdempotentRerun(t *testing.T) {
	const tokenEnv = "GOOBERS_CONNECT_TEST_TOKEN"
	t.Setenv(tokenEnv, "test-token")
	seeder := stubConnectSeeder(t)
	root := connectTestInstance(t, "quickstart")

	run := func() onboardingActionResult {
		t.Helper()
		code, stdout, stderr := runArgs(t, "connect", "acme/web", "--token-env", tokenEnv, "--seed", "--json", root)
		if code != 0 {
			t.Fatalf("connect --seed: code=%d stderr=%q", code, stderr)
		}
		return connectEnvelope(t, stdout)
	}

	first := run()
	if !slices.Contains(first.Created, "issue:"+connectSeedIssueID) {
		t.Fatalf("first created = %v", first.Created)
	}
	second := run()
	if len(second.Created) != 0 || len(second.Updated) != 0 {
		t.Fatalf("second run created=%v updated=%v, want none", second.Created, second.Updated)
	}
	for _, want := range []string{"label:goobers:approved", "label:goobers:ready", "issue:" + connectSeedIssueID} {
		if !slices.Contains(second.Skipped, want) {
			t.Errorf("second skipped lacks %q: %v", want, second.Skipped)
		}
	}
	if len(seeder.createRequests) != 1 {
		t.Fatalf("rerun created additional issues: %d total", len(seeder.createRequests))
	}
}

func TestConnectSeedPendingWithoutToken(t *testing.T) {
	const tokenEnv = "GOOBERS_CONNECT_TEST_TOKEN"
	if original, had := os.LookupEnv(tokenEnv); had {
		t.Cleanup(func() { _ = os.Setenv(tokenEnv, original) })
		_ = os.Unsetenv(tokenEnv)
	}
	called := false
	previous := newOnboardingIssueSeeder
	newOnboardingIssueSeeder = func(string) onboardingIssueSeeder {
		called = true
		return nil
	}
	t.Cleanup(func() { newOnboardingIssueSeeder = previous })

	root := connectTestInstance(t, "quickstart")
	code, stdout, stderr := runArgs(t, "connect", "acme/web", "--token-env", tokenEnv, "--seed", "--json", root)
	if code != 0 {
		t.Fatalf("connect --seed without token: code=%d stderr=%q", code, stderr)
	}
	if called {
		t.Fatal("provider was constructed without credentials")
	}
	result := connectEnvelope(t, stdout)
	if !slices.Contains(result.Skipped, "issue:"+connectSeedIssueID+" (pending: credentials unavailable)") {
		t.Fatalf("skipped = %v", result.Skipped)
	}
	if !strings.Contains(result.NextCommand, "goobers validate --check-harness --check-repos") {
		t.Fatalf("nextCommand = %q", result.NextCommand)
	}
}

func TestConnectRepoCheckFailureFoldsIntoOutput(t *testing.T) {
	const tokenEnv = "GOOBERS_CONNECT_TEST_TOKEN"
	t.Setenv(tokenEnv, "test-token")
	stubConnectReachability(t, os.ErrDeadlineExceeded)

	root := connectTestInstance(t, "quickstart")
	code, _, stderr := runArgs(t, "connect", "acme/web", "--token-env", tokenEnv, root)
	if code != 1 {
		t.Fatalf("connect with unreachable repo: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stderr, "acme/web is not reachable with the credential named by "+tokenEnv) {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestConnectSeedEnsuresClaimMirrorLabel reproduces the shipped half of
// cold-start python #7: the quickstart's query-backlog stage claims, so a first
// run writes the goobers:claimed mirror — a label --seed used not to create.
// The starter issue must NOT carry it, or the selectors that are supposed to
// find that issue would exclude it as already claimed.
func TestConnectSeedEnsuresClaimMirrorLabel(t *testing.T) {
	const tokenEnv = "GOOBERS_CONNECT_TEST_TOKEN"
	t.Setenv(tokenEnv, "test-token")
	seeder := stubConnectSeeder(t)
	root := connectTestInstance(t, "quickstart")

	code, stdout, stderr := runArgs(t, "connect", "acme/web", "--token-env", tokenEnv, "--seed", "--json", root)
	if code != 0 {
		t.Fatalf("connect --seed: code=%d stderr=%q", code, stderr)
	}
	result := connectEnvelope(t, stdout)
	for _, want := range []string{
		"label:goobers:approved",
		"label:goobers:ready",
		"label:" + providers.LabelClaimed,
		"issue:" + connectSeedIssueID,
	} {
		if !slices.Contains(result.Created, want) {
			t.Errorf("created lacks %q: %v", want, result.Created)
		}
	}
	if len(seeder.createRequests) != 1 {
		t.Fatalf("created issues = %d, want 1", len(seeder.createRequests))
	}
	wantIssueLabels := []string{"goobers:approved", "goobers:ready"}
	if !slices.Equal(seeder.createRequests[0].Labels, wantIssueLabels) {
		t.Fatalf("issue labels = %v, want %v (lifecycle labels are ensured, never applied)",
			seeder.createRequests[0].Labels, wantIssueLabels)
	}
}

// TestConnectDerivedLabelsCoverWorkflowAppliedLabels is cold-start python #7
// verbatim: two canonical-shaped workflows whose selector derivation used to
// yield only goobers:approved + goobers:ready, leaving the five labels the
// workflows actually write or exclude on uncreated — so the first park or
// close-out would have died on a label GitHub does not have.
func TestConnectDerivedLabelsCoverWorkflowAppliedLabels(t *testing.T) {
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{
			ObjectMeta: metav1.ObjectMeta{Name: "python-service"},
			Spec: apiv1.GaggleSpec{
				Project: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
				Backlog: apiv1.BacklogRef{Project: "acme/web"},
			},
		}},
		Workflows: []apiv1.Workflow{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "backlog-curation"},
				Spec: apiv1.WorkflowSpec{
					Gaggle: "python-service",
					Tasks: []apiv1.Task{{
						Name: "query-backlog",
						Type: apiv1.TaskDeterministic,
						Run:  &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query"}},
						Inputs: map[string]string{
							"trustLabel":    "goobers:approved",
							"excludeLabels": "goobers:ready,goobers:needs-human,goobers:blocked-on-sibling,goobers:needs-remediation",
						},
					}},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "implementation"},
				Spec: apiv1.WorkflowSpec{
					Gaggle: "python-service",
					Tasks: []apiv1.Task{
						{
							Name:   "query-backlog",
							Type:   apiv1.TaskDeterministic,
							Run:    &apiv1.DeterministicRun{Command: []string{"goobers", "backlog-query", "--claim"}},
							Inputs: map[string]string{"trustLabel": "goobers:approved", "requireLabels": "goobers:ready", "excludeLabels": "goobers/status:in-review"},
						},
						{
							Name:   "close-out",
							Type:   apiv1.TaskDeterministic,
							Run:    &apiv1.DeterministicRun{Command: []string{"goobers", "issue-close-out"}},
							Inputs: map[string]string{"status": "in-review"},
						},
						{
							Name:   "park-escalated",
							Type:   apiv1.TaskDeterministic,
							Run:    &apiv1.DeterministicRun{Command: []string{"goobers", "issue-close-out"}},
							Inputs: map[string]string{"status": "needs-remediation"},
						},
						{
							Name:   "park-needs-human",
							Type:   apiv1.TaskDeterministic,
							Run:    &apiv1.DeterministicRun{Command: []string{"goobers", "issue-close-out"}},
							Inputs: map[string]string{"status": "needs-human"},
						},
					},
				},
			},
		},
	}

	selectors, applied, workflow := connectDerivedLabels(set, "acme", "web")
	wantSelectors := []string{"goobers:approved", "goobers:ready"}
	if !slices.Equal(selectors, wantSelectors) {
		t.Fatalf("selectors = %v, want %v", selectors, wantSelectors)
	}
	// The exact five labels cold-start python #7 had to create by hand.
	wantApplied := []string{
		"goobers/status:in-review",
		"goobers:blocked-on-sibling",
		"goobers:claimed",
		"goobers:needs-human",
		"goobers:needs-remediation",
	}
	if !slices.Equal(applied, wantApplied) {
		t.Fatalf("applied = %v, want %v", applied, wantApplied)
	}
	if workflow != "backlog-curation" {
		t.Fatalf("workflow = %q", workflow)
	}
	for _, selector := range selectors {
		if slices.Contains(applied, selector) {
			t.Fatalf("applied set repeats selector %q", selector)
		}
	}
}

// TestConnectDerivedLabelsStayQuietForSelectorOnlyConfig is the negative case:
// a gaggle whose workflow neither claims, parks, nor excludes derives no
// lifecycle labels at all, so the widened derivation cannot create clutter in
// a repository that does not need it.
func TestConnectDerivedLabelsStayQuietForSelectorOnlyConfig(t *testing.T) {
	set := &instance.ConfigSet{
		Gaggles: []apiv1.Gaggle{{
			ObjectMeta: metav1.ObjectMeta{Name: "example"},
			Spec: apiv1.GaggleSpec{
				Project:       apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
				Backlog:       apiv1.BacklogRef{Project: "acme/web", Labels: []string{"goobers"}},
				RequireLabels: []string{"goobers:ready"},
			},
		}},
		Workflows: []apiv1.Workflow{{
			ObjectMeta: metav1.ObjectMeta{Name: "docs-only"},
			Spec: apiv1.WorkflowSpec{
				Gaggle: "example",
				Tasks: []apiv1.Task{{
					Name:   "open-pr",
					Type:   apiv1.TaskDeterministic,
					Run:    &apiv1.DeterministicRun{Command: []string{"goobers", "open-pr"}},
					Inputs: map[string]string{"resultFile": "pr-result.json"},
				}},
			},
		}},
	}
	selectors, applied, _ := connectDerivedLabels(set, "acme", "web")
	if !slices.Equal(selectors, []string{"goobers", "goobers:ready"}) {
		t.Fatalf("selectors = %v", selectors)
	}
	if len(applied) != 0 {
		t.Fatalf("applied = %v, want none", applied)
	}
}

// TestConnectReportsSelectorRealityMismatch is the connect-time reality echo
// (cold-start ado #5): the config is valid and the repository reachable, but
// no open issue carries what the selectors require, so every scheduled run
// would claim nothing.
func TestConnectReportsSelectorRealityMismatch(t *testing.T) {
	const tokenEnv = "GOOBERS_CONNECT_TEST_TOKEN"
	t.Setenv(tokenEnv, "test-token")
	seeder := stubConnectSeeder(t)
	seeder.items = []providers.WorkItem{
		{ID: "1", Title: "bug: report totals", Labels: []string{"bug", "reporting"}},
		{ID: "2", Title: "cli: add --json", Labels: []string{"cli", "enhancement"}},
		{ID: "3", Title: "half-eligible", Labels: []string{"goobers:approved"}},
	}
	root := connectTestInstance(t, "quickstart")

	code, stdout, stderr := runArgs(t, "connect", "acme/web", "--token-env", tokenEnv, root)
	if code != 0 {
		t.Fatalf("connect: code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"note: " + connectSelectorRealityCode,
		"backlog selectors (goobers:approved, goobers:ready) match 0 of 3 open issues in acme/web",
		"claim nothing",
		"connect --seed",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

// TestConnectSelectorRealitySilentWhenMatched is the negative case: one
// eligible open issue is enough, and a clean connect stays quiet.
func TestConnectSelectorRealitySilentWhenMatched(t *testing.T) {
	const tokenEnv = "GOOBERS_CONNECT_TEST_TOKEN"
	t.Setenv(tokenEnv, "test-token")
	seeder := stubConnectSeeder(t)
	seeder.items = []providers.WorkItem{
		{ID: "1", Title: "unrelated", Labels: []string{"bug"}},
		{ID: "2", Title: "eligible", Labels: []string{"goobers:approved", "goobers:ready"}},
	}
	root := connectTestInstance(t, "quickstart")

	code, stdout, stderr := runArgs(t, "connect", "acme/web", "--token-env", tokenEnv, root)
	if code != 0 {
		t.Fatalf("connect: code=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, connectSelectorRealityCode) || strings.Contains(stderr, connectSelectorRealityCode) {
		t.Fatalf("clean connect emitted a selector note:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
}

// TestConnectSelectorRealityNoteGoesToStderrUnderJSON keeps the versioned
// envelope machine-parseable while still telling a scripted caller.
func TestConnectSelectorRealityNoteGoesToStderrUnderJSON(t *testing.T) {
	const tokenEnv = "GOOBERS_CONNECT_TEST_TOKEN"
	t.Setenv(tokenEnv, "test-token")
	seeder := stubConnectSeeder(t)
	seeder.items = []providers.WorkItem{{ID: "1", Labels: []string{"bug"}}}
	root := connectTestInstance(t, "quickstart")

	code, stdout, stderr := runArgs(t, "connect", "acme/web", "--token-env", tokenEnv, "--json", root)
	if code != 0 {
		t.Fatalf("connect: code=%d stderr=%q", code, stderr)
	}
	connectEnvelope(t, stdout) // stdout must still decode as the envelope alone
	if !strings.Contains(stderr, connectSelectorRealityCode) {
		t.Fatalf("stderr lacks the selector note: %q", stderr)
	}
}

// TestConnectRestoresInstanceWhenValidationFails proves the atomic-restore
// half of the contract: a connect that cannot be vouched for leaves no
// half-written tree behind, so the placeholder is still there for the next
// attempt.
func TestConnectRestoresInstanceWhenValidationFails(t *testing.T) {
	root := connectTestInstance(t, "quickstart")
	// Break a file connect never touches, so validation fails only after the
	// rewrite has already been written.
	gooberFile := filepath.Join(root, "config", "gaggles", "example", "goobers", "implementer", "goober.yaml")
	if err := os.WriteFile(gooberFile, []byte("apiVersion: goobers.dev/v1alpha1\nkind: Goober\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configFile := instance.NewLayout(root).ConfigFile()
	beforeConfig, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	gaggleFile := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	beforeGaggle, err := os.ReadFile(gaggleFile)
	if err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runArgs(t, "connect", "acme/web", root)
	if code == 0 {
		t.Fatalf("connect succeeded against an invalid instance; stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "connected instance failed validation") ||
		!strings.Contains(stderr, "nothing was left changed") {
		t.Fatalf("stderr = %q", stderr)
	}
	afterConfig, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterConfig) != string(beforeConfig) {
		t.Fatalf("instance.yaml not restored:\n%s", afterConfig)
	}
	afterGaggle, err := os.ReadFile(gaggleFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterGaggle) != string(beforeGaggle) {
		t.Fatalf("gaggle.yaml not restored:\n%s", afterGaggle)
	}
}

func TestConnectFlagsAfterPositionals(t *testing.T) {
	root := connectTestInstance(t, "quickstart")
	t.Setenv("MY_TOKEN", "")
	code, stdout, stderr := runArgs(t, "connect", "acme/web", root, "--json", "--token-env", "MY_TOKEN")
	if code != 0 {
		t.Fatalf("connect: code=%d stderr=%q", code, stderr)
	}
	result := connectEnvelope(t, stdout)
	if !slices.Contains(result.Updated, "instance.yaml") {
		t.Fatalf("updated = %v", result.Updated)
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repos[0].Token.Env != "MY_TOKEN" {
		t.Fatalf("token env = %q", cfg.Repos[0].Token.Env)
	}
}
