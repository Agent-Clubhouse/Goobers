package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
)

func TestConnectADOPreflightFailurePreservesFiles(t *testing.T) {
	t.Setenv("GOOBERS_ADO_TOKEN", "test-pat")
	stubConnectReachability(t, errors.New("repository access denied"))
	root := filepath.Join(t.TempDir(), "ado")
	code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	paths, err := filepath.Glob(filepath.Join(root, "config", "gaggles", "*", "gaggle.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("gaggles: %v %v", paths, err)
	}
	paths = append(paths, instance.NewLayout(root).ConfigFile())
	before := make(map[string]string)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = string(data)
	}
	code, _, stderr = runArgs(t, "connect", "contoso/boards/web", "--token-env", "GOOBERS_ADO_TOKEN", root)
	if code != 1 {
		t.Fatalf("connect: %d %s", code, stderr)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != before[path] {
			t.Fatalf("failed preflight changed %s: %v", path, err)
		}
	}
}

func TestConnectADORoundTrip(t *testing.T) {
	t.Setenv("GOOBERS_ADO_TOKEN", "")
	root := filepath.Join(t.TempDir(), "ado")
	code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	for attempt := range 2 {
		code, _, stderr = runArgs(t, "connect", "contoso/boards/web", "--token-env", "GOOBERS_ADO_TOKEN", "--replace", root)
		if code != 0 {
			t.Fatalf("connect attempt %d: %d %s", attempt, code, stderr)
		}
		cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Repos) != 1 || cfg.Repos[0].Provider != "ado" || cfg.Repos[0].Owner != "contoso" || cfg.Repos[0].Project != "boards" || cfg.Repos[0].Name != "web" || cfg.Repos[0].Token.Env != "GOOBERS_ADO_TOKEN" {
			t.Fatalf("incorrect repository: %+v", cfg.Repos)
		}
		set, report, err := instance.LoadConfigDir(filepath.Join(root, "config"))
		if err != nil || report.HasErrors() {
			t.Fatalf("load: %v %+v", err, report)
		}
		if len(set.Gaggles) != 1 || set.Gaggles[0].Spec.Project.Owner != "contoso" || set.Gaggles[0].Spec.Project.Project != "boards" || set.Gaggles[0].Spec.Project.Name != "web" || set.Gaggles[0].Spec.Backlog.Project != "boards" {
			t.Fatalf("incorrect gaggle: %+v", set.Gaggles)
		}
	}
	before, err := os.ReadFile(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runArgs(t, "connect", "contoso/boards/web", "--token-env", "GOOBERS_ADO_TOKEN", root)
	after, err := os.ReadFile(instance.NewLayout(root).ConfigFile())
	if code != 0 || err != nil || string(before) != string(after) {
		t.Fatalf("repeat changed configuration: code=%d err=%v stderr=%s", code, err, stderr)
	}
}

// init defaults an ADO repository to azure-cli authentication; connect keeps
// that (and the other non-PAT sources) unless --token-env is given
// explicitly, and with a kept source it neither preflights nor seeds through
// a token variable the instance does not name.
func TestConnectADOKeepsNonPATAuthUnlessTokenEnvGiven(t *testing.T) {
	for _, kind := range []string{"", instance.ADOAuthWorkloadIdentity, instance.ADOAuthManagedIdentity} {
		want := kind
		if want == "" {
			want = instance.ADOAuthAzureCLI
		}
		t.Run(want, func(t *testing.T) {
			// A set token variable and a failing preflight prove that a kept
			// source neither reads the variable nor runs the token preflight.
			t.Setenv("GOOBERS_ADO_TOKEN", "test-pat")
			stubConnectReachability(t, errors.New("token preflight must not run"))
			root := filepath.Join(t.TempDir(), "ado")
			args := []string{"init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8"}
			if kind != "" {
				args = append(args, "--repo-auth-kind="+kind)
			}
			if code, _, stderr := runArgs(t, append(args, root)...); code != 0 {
				t.Fatalf("init: %d %s", code, stderr)
			}
			configFile := instance.NewLayout(root).ConfigFile()
			repo := func() instance.RepoRef {
				cfg, err := instance.LoadConfig(configFile)
				if err != nil || len(cfg.Repos) != 1 {
					t.Fatalf("load: %v %+v", err, cfg)
				}
				return cfg.Repos[0]
			}
			for attempt := range 2 {
				code, stdout, stderr := runArgs(t, "connect", "contoso/boards/web", root)
				if code != 0 {
					t.Fatalf("connect attempt %d: %d %s", attempt, code, stderr)
				}
				got := repo()
				if got.Owner != "contoso" || got.Project != "boards" || got.Name != "web" || got.Auth == nil || got.Auth.Kind != want || got.Token.Env != "" {
					t.Fatalf("connect attempt %d did not keep %s auth: %+v auth=%+v", attempt, want, got, got.Auth)
				}
				if attempt == 0 && !strings.Contains(stdout, "kept     "+want+" authentication") {
					t.Fatalf("connect did not report the kept auth kind:\n%s", stdout)
				}
			}
			code, _, stderr := runArgs(t, "connect", "contoso/boards/web", "--token-env", "ADO_PAT", root)
			if code != 1 || !strings.Contains(stderr, "already has a different credential source; use --replace") {
				t.Fatalf("explicit --token-env on a connected repository: %d %s", code, stderr)
			}
			stubConnectReachability(t, nil)
			t.Setenv("ADO_PAT", "test-pat")
			if code, _, stderr := runArgs(t, "connect", "contoso/boards/web", "--token-env", "ADO_PAT", "--replace", root); code != 0 {
				t.Fatalf("switch to PAT: %d %s", code, stderr)
			}
			if got := repo(); got.Auth == nil || got.Auth.Kind != instance.ADOAuthPAT || got.Token.Env != "ADO_PAT" {
				t.Fatalf("explicit --token-env did not switch to PAT: %+v auth=%+v", got, got.Auth)
			}
		})
	}
}

// A PAT repository keeps its own token variable unless --token-env is given:
// connect, a repeat, and --replace onto another repository all leave MY_PAT
// in place, and the token preflight reads MY_PAT, not the default variable.
func TestConnectADOKeepsPATTokenVariableUnlessTokenEnvGiven(t *testing.T) {
	t.Setenv("GOOBERS_ADO_TOKEN", "")
	t.Setenv("MY_PAT", "my-pat")
	var preflights []string
	previous := targetRepositoryReachable
	targetRepositoryReachable = func(_ context.Context, repo instance.RepoRef, token string, _ credentials.StoreResolver) error {
		preflights = append(preflights, repo.Name+":"+repo.Token.Env+":"+token)
		return nil
	}
	t.Cleanup(func() { targetRepositoryReachable = previous })
	stubADOAccessReads(t, fullADORepositoryAccess(), fullADOBacklogStates())
	root := filepath.Join(t.TempDir(), "ado")
	code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--repo-token-env=MY_PAT", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	for _, args := range [][]string{
		{"connect", "contoso/boards/web", root},
		{"connect", "contoso/boards/web", root},
		{"connect", "contoso/boards/api", "--replace", root},
	} {
		if code, _, stderr := runArgs(t, args...); code != 0 {
			t.Fatalf("%v: %d %s", args, code, stderr)
		}
		cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Repos[0]; got.Token.Env != "MY_PAT" || connectADOAuthLabel(got) != instance.ADOAuthPAT {
			t.Fatalf("%v rewrote the PAT source: %+v auth=%+v", args, got, got.Auth)
		}
	}
	if len(preflights) == 0 || strings.Join(preflights, " ") != strings.Repeat("web:MY_PAT:my-pat ", 2)+"api:MY_PAT:my-pat" {
		t.Fatalf("preflights = %q, want each connect to check with MY_PAT", preflights)
	}
}

// A PAT read from a file names no token variable: connect keeps it, and the
// later steps treat it as a kept source rather than reading --token-env.
func TestConnectADOKeepsNonEnvPATSource(t *testing.T) {
	file := instance.RepoRef{Provider: "ado", Owner: "your-org", Project: "your-project", Name: "your-repo",
		Token: instance.TokenRef{File: "/run/secrets/ado-pat"}, Auth: &instance.RepoAuthConfig{Kind: instance.ADOAuthPAT}}
	cfg := &instance.Config{Repos: []instance.RepoRef{file}}
	opts := connectOptions{ado: &connectADORepo{Organization: "org", Project: "boards", Repository: "web"}, owner: "org", name: "web", tokenEnv: "GOOBERS_ADO_TOKEN"}
	if changed, err := connectRewriteADOInstanceConfig(cfg, opts); err != nil || !changed {
		t.Fatalf("rewrite: changed=%v err=%v", changed, err)
	}
	if got := cfg.Repos[0]; got.Token.File != "/run/secrets/ado-pat" || got.Token.Env != "" {
		t.Fatalf("file-backed PAT not kept: %+v", got.Token)
	}
	if changed, err := connectRewriteADOInstanceConfig(cfg, opts); err != nil || changed {
		t.Fatalf("repeat changed the entry: changed=%v err=%v", changed, err)
	}
	kept := connectKeptADOCredential(cfg, opts)
	if kept.keptADOAuth == nil || kept.keptADOAuth.Token.File != "/run/secrets/ado-pat" {
		t.Fatalf("kept source = %+v, want the file-backed PAT entry", kept.keptADOAuth)
	}
	// A copied (not aliased) auth block with the same value is the same source.
	copied := cfg.Repos[0]
	auth := *copied.Auth
	copied.Auth = &auth
	if !connectADOSameCredential(cfg.Repos[0], copied) {
		t.Fatal("equal auth blocks compared unequal")
	}
}

// An explicit --token-env on the init placeholder records PAT
// authentication straight away.
func TestConnectADOExplicitTokenEnvRecordsPAT(t *testing.T) {
	t.Setenv("ADO_PAT", "")
	root := filepath.Join(t.TempDir(), "ado")
	code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	if code, _, stderr = runArgs(t, "connect", "contoso/boards/web", "--token-env", "ADO_PAT", root); code != 0 {
		t.Fatalf("connect: %d %s", code, stderr)
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Repos[0]; got.Auth == nil || got.Auth.Kind != instance.ADOAuthPAT || got.Token.Env != "ADO_PAT" {
		t.Fatalf("repository = %+v auth=%+v, want PAT reading ADO_PAT", got, got.Auth)
	}
}

func TestConnectADORewritesPlaceholderDisplayName(t *testing.T) {
	t.Setenv("GOOBERS_ADO_TOKEN", "")
	root := filepath.Join(t.TempDir(), "ado")
	code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	code, _, stderr = runArgs(t, "connect", "https://dev.azure.com/contoso/My%20Project/_git/widgets", root)
	if code != 0 {
		t.Fatalf("connect: %d %s", code, stderr)
	}
	gaggleFile := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	gaggleData, err := os.ReadFile(gaggleFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(gaggleData)
	if got := connectTestGaggleDisplayName(t, gaggleFile); got != "contoso/My Project/widgets" || strings.Contains(text, "your-org/your-project/your-repo") {
		t.Fatalf("ADO gaggle displayName not rewritten:\n%s", text)
	}
	var out strings.Builder
	if code := runValidate([]string{root}, &out, &out); code != 0 || strings.Contains(out.String(), placeholderFindingCode) {
		t.Fatalf("validate after connect: code=%d output=%s", code, out.String())
	}
}

func TestConnectADOPreservesCustomDisplayNameUnlessReplace(t *testing.T) {
	t.Setenv("GOOBERS_ADO_TOKEN", "")
	root := filepath.Join(t.TempDir(), "ado")
	code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	gaggleFile := filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml")
	connectTestSetGaggleDisplayName(t, gaggleFile, "Custom ADO widgets")
	code, _, stderr = runArgs(t, "connect", "https://dev.azure.com/contoso/My%20Project/_git/widgets", root)
	if code != 0 {
		t.Fatalf("connect: %d %s", code, stderr)
	}
	connected, err := os.ReadFile(gaggleFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := connectTestGaggleDisplayName(t, gaggleFile); got != "Custom ADO widgets" {
		t.Fatalf("connect without --replace rewrote custom displayName:\n%s", connected)
	}
	code, _, stderr = runArgs(t, "connect", "https://dev.azure.com/fabrikam/Other%20Project/_git/gears", "--replace", root)
	if code != 0 {
		t.Fatalf("connect --replace: %d %s", code, stderr)
	}
	replaced, err := os.ReadFile(gaggleFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := connectTestGaggleDisplayName(t, gaggleFile); got != "fabrikam/Other Project/gears" {
		t.Fatalf("connect --replace did not rewrite displayName:\n%s", replaced)
	}
}

func TestADOOnboardingExampleValidatesWithScaffold(t *testing.T) {
	t.Setenv("GOOBERS_ADO_TOKEN", "")
	root := filepath.Join(t.TempDir(), "ado")
	code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	code, _, stderr = runArgs(t, "connect", "contoso/platform/widgets", root)
	if code != 0 {
		t.Fatalf("connect: %d %s", code, stderr)
	}
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "ado-onboarding", "gaggle.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config", "gaggles", "example", "gaggle.yaml"), example, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runArgs(t, "validate", root)
	if code != 0 {
		t.Fatalf("example invalid: %d stdout=%s stderr=%s", code, stdout, stderr)
	}
}
