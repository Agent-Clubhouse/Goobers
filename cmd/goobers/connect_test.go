package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
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
	for _, bad := range []string{"acme", "acme/web/extra", "https://gitlab.com/acme/web"} {
		code, _, stderr := runArgs(t, "connect", bad, root)
		if code != 2 {
			t.Fatalf("connect %q code = %d, want 2; stderr=%q", bad, code, stderr)
		}
		if !strings.Contains(stderr, "GitHub is the only supported provider in v1") {
			t.Fatalf("stderr = %q", stderr)
		}
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
