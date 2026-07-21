package executor

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/procenv"
)

// TestBaseEnvMatchesProcenv is the #248 drift-guard: executor's baseEnv()
// must be exactly procenv.BaseEnv() — the shared definition harness's
// baseEnv() also delegates to — not a local copy that can silently diverge
// again the way #98/#122 did.
func TestBaseEnvMatchesProcenv(t *testing.T) {
	t.Setenv("GOPROXY", "https://proxy.example.internal")
	t.Setenv("LC_ALL", "C")

	got := append([]string(nil), baseEnv(nil)...)
	want := append([]string(nil), procenv.BaseEnv()...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("executor baseEnv() diverged from procenv.BaseEnv():\n got:  %v\n want: %v", got, want)
	}
}

// TestBaseEnvPassesThroughExtendedAllowlist is the regression test for #122's
// allowlist-parity fix: internal/harness's #75/#98 extension (XDG base dirs,
// locale, TLS/proxy config) must also apply to internal/executor's identical
// SEC-045 allowlist, not just PATH/HOME/TMPDIR.
func TestBaseEnvPassesThroughExtendedAllowlist(t *testing.T) {
	extended := map[string]string{
		"XDG_CONFIG_HOME": "/home/tester/.config",
		"XDG_DATA_HOME":   "/home/tester/.local/share",
		"LANG":            "en_US.UTF-8",
		"LC_ALL":          "C",
		"SSL_CERT_FILE":   "/etc/ssl/certs/custom-ca.pem",
		"HTTP_PROXY":      "http://proxy.example.internal:8080",
		"HTTPS_PROXY":     "https://proxy.example.internal:8443",
		"NO_PROXY":        "localhost,127.0.0.1",
	}
	for name, value := range extended {
		t.Setenv(name, value)
	}

	env := baseEnv(nil)
	for name, value := range extended {
		want := name + "=" + value
		found := false
		for _, kv := range env {
			if kv == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s did not pass through baseEnv(), got %v", name, env)
		}
	}
}

// TestBaseEnvStillBlocksSecretShapedVars proves the extension stays
// default-deny: a var that merely resembles an allowlisted name (shares a
// prefix substring) must not pass, only exact allowlisted names and the LC_*
// family do.
func TestBaseEnvStillBlocksSecretShapedVars(t *testing.T) {
	blocked := map[string]string{
		"AWS_SECRET_ACCESS_KEY":    "not-a-real-secret-but-should-never-pass",
		"LANGUAGE_MODEL_API_KEY":   "should-not-pass-either",
		"LOCALE_LC_OVERRIDE_TOKEN": "should-not-pass",
	}
	for name, value := range blocked {
		t.Setenv(name, value)
	}
	t.Setenv("LANG", "en_US.UTF-8")

	env := baseEnv(nil)
	for name := range blocked {
		for _, kv := range env {
			if strings.HasPrefix(kv, name+"=") {
				t.Fatalf("blocked var %s leaked into baseEnv(): %v", name, env)
			}
		}
	}
	foundLang := false
	for _, kv := range env {
		if kv == "LANG=en_US.UTF-8" {
			foundLang = true
		}
	}
	if !foundLang {
		t.Fatalf("expected the exact allowlisted LANG to still pass through, got %v", env)
	}
}

// TestBuildStageEnv_InjectsRunContextOnlyWhenRequested is #322's core: the
// run's operational identity (GOOBERS_RUN_ID/GOOBERS_GAGGLE/
// GOOBERS_WORKFLOW/GOOBERS_INSTANCE_ROOT) is injected only when
// injectRunContext is set — i.e. only for a stage whose command is the goobers
// CLI. A stage that runs the project's own build/test suite gets a clean env,
// closing the leak at the source. Declared inputs (GOOBERS_INPUT_*) are the
// stage's own config and flow to every stage kind regardless. injector is nil
// (no declared caps), so buildStageEnv returns before the credential/registrar
// path.
func TestBuildStageEnv_InjectsRunContextOnlyWhenRequested(t *testing.T) {
	inputs := map[string]interface{}{"trustLabel": "goobers:approved"}

	withCtx, err := buildStageEnv(context.Background(), nil, nil, nil, "run-123", "alpha", "implementation", "goobers/", "/instances/demo", true, inputs, map[string]string{"FEATURE_FLAG": "enabled"}, nil)
	if err != nil {
		t.Fatalf("buildStageEnv(injectRunContext=true): %v", err)
	}
	for _, want := range []string{
		"GOOBERS_RUN_ID=run-123",
		"GOOBERS_GAGGLE=alpha",
		"GOOBERS_WORKFLOW=implementation",
		"GOOBERS_BRANCH_NAMESPACE=goobers/",
		"GOOBERS_INSTANCE_ROOT=/instances/demo",
		"GOOBERS_INPUT_TRUSTLABEL=goobers:approved",
		"FEATURE_FLAG=enabled",
	} {
		if !hasEnv(withCtx, want) {
			t.Errorf("injectRunContext=true: missing %q in %v", want, withCtx)
		}
	}

	noCtx, err := buildStageEnv(context.Background(), nil, nil, nil, "run-123", "alpha", "implementation", "goobers/", "/instances/demo", false, inputs, nil, nil)
	if err != nil {
		t.Fatalf("buildStageEnv(injectRunContext=false): %v", err)
	}
	for _, prefix := range []string{"GOOBERS_RUN_ID", "GOOBERS_GAGGLE", "GOOBERS_WORKFLOW", "GOOBERS_BRANCH_NAMESPACE", "GOOBERS_INSTANCE_ROOT"} {
		if hasEnvPrefix(noCtx, prefix) {
			t.Errorf("injectRunContext=false: %s leaked into %v", prefix, noCtx)
		}
	}
	// The declared input still flows — it's the stage's config, not run identity.
	if !hasEnv(noCtx, "GOOBERS_INPUT_TRUSTLABEL=goobers:approved") {
		t.Errorf("injectRunContext=false: declared input dropped, want it kept: %v", noCtx)
	}

	// Even when requested, GOOBERS_INSTANCE_ROOT is omitted if unset (empty
	// instanceRoot — see ShellExecutor.InstanceRoot), preserving prior behavior.
	emptyRoot, err := buildStageEnv(context.Background(), nil, nil, nil, "run-123", "alpha", "implementation", "", "", true, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildStageEnv(empty instanceRoot): %v", err)
	}

	if hasEnvPrefix(emptyRoot, "GOOBERS_INSTANCE_ROOT") {
		t.Errorf("empty instanceRoot should omit GOOBERS_INSTANCE_ROOT, got %v", emptyRoot)
	}
	// An empty branchNamespace is likewise omitted rather than injected as a
	// bare GOOBERS_BRANCH_NAMESPACE= — the goobers-CLI stage's default resolver
	// (providers.DefaultBranchNamespace) then applies.
	if hasEnvPrefix(emptyRoot, "GOOBERS_BRANCH_NAMESPACE") {
		t.Errorf("empty branchNamespace should omit GOOBERS_BRANCH_NAMESPACE, got %v", emptyRoot)
	}
}

// TestBuildStageEnvPassesExtraAllowlist is #736's executor path: an
// instance-declared passthrough name (ShellExecutor.ExtraEnvAllowlist →
// buildStageEnv) reaches the stage env additively, while an undeclared ambient
// var stays absent — default-deny preserved.
func TestBuildStageEnvPassesExtraAllowlist(t *testing.T) {
	t.Setenv("MY_TOOLCHAIN_HOME", "/opt/toolchain")
	t.Setenv("MY_UNDECLARED_SECRET", "should-not-pass")

	env, err := buildStageEnv(context.Background(), nil, nil, nil, "", "", "", "", "", false, nil, nil, []string{"MY_TOOLCHAIN_HOME"})
	if err != nil {
		t.Fatalf("buildStageEnv: %v", err)
	}
	if !hasEnv(env, "MY_TOOLCHAIN_HOME=/opt/toolchain") {
		t.Errorf("extra-allowlisted var missing from stage env: %v", env)
	}
	if hasEnvPrefix(env, "MY_UNDECLARED_SECRET") {
		t.Errorf("undeclared ambient var leaked into stage env: %v", env)
	}
}

func TestBuildStageEnvIncludesDeclaredEnvironment(t *testing.T) {
	env, err := buildStageEnv(
		context.Background(),
		nil,
		nil,
		nil,
		"",
		"",
		"",
		"",
		"",
		false,
		nil,
		map[string]string{"FEATURE_FLAG": "enabled", "EMPTY_VALUE": ""},
		nil,
	)
	if err != nil {
		t.Fatalf("buildStageEnv: %v", err)
	}
	for _, want := range []string{"FEATURE_FLAG=enabled", "EMPTY_VALUE="} {
		if !hasEnv(env, want) {
			t.Errorf("declared environment missing %q from %v", want, env)
		}
	}
}

// TestStageInvokesGoobersCLI pins the single discriminator that gates both
// SelfBin substitution and run-context injection (#322): a goobers-CLI stage
// vs an external-tool stage (make/go/git) — including the project's own
// build/test stages, which must NOT be treated as goobers-CLI stages.
func TestStageInvokesGoobersCLI(t *testing.T) {
	cases := []struct {
		name string
		cmd  []string
		want bool
	}{
		{"backlog-query", []string{"goobers", "backlog-query", "--claim"}, true},
		{"open-pr", []string{"goobers", "open-pr"}, true},
		{"ci-poll", []string{"goobers", "ci-poll", "--wait"}, true},
		{"make ci", []string{"make", "ci"}, false},
		{"make test", []string{"make", "test"}, false},
		{"sh -c", []string{"sh", "-c", "echo hi"}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := stageInvokesGoobersCLI(c.cmd); got != c.want {
			t.Errorf("%s: stageInvokesGoobersCLI(%v) = %v, want %v", c.name, c.cmd, got, c.want)
		}
	}
}

func TestStageInvokesProviderBuiltin(t *testing.T) {
	cases := []struct {
		name string
		cmd  []string
		want bool
	}{
		{"backlog query", []string{"goobers", "backlog-query", "--claim"}, true},
		{"open pr", []string{"goobers", "open-pr"}, true},
		{"merge pr", []string{"goobers", "merge-pr"}, true},
		// #884: merge-queue-poll is a provider-chain subcommand like every
		// other entry here. Omitting it cost it both the transient-stderr
		// reclassification and the infrastructure-attempt retry budget, so
		// a single transient blip while watching a merge queue failed the
		// whole merge-review run.
		{"merge queue poll", []string{"goobers", "merge-queue-poll"}, true},
		{"elect lander", []string{"goobers", "elect-lander"}, true},
		{"reconcile post merge", []string{"goobers", "reconcile-post-merge"}, true},
		{"update behind pr", []string{"goobers", "update-behind-pr"}, true},
		{"ci poll uses in-process classification", []string{"goobers", "ci-poll"}, false},
		{"push branch is git-backed", []string{"goobers", "push-branch"}, false},
		{"telemetry query", []string{"goobers", "telemetry-query"}, false},
		{"external command", []string{"make", "ci"}, false},
		{"missing subcommand", []string{"goobers"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stageInvokesProviderBuiltin(tc.cmd); got != tc.want {
				t.Fatalf("stageInvokesProviderBuiltin(%v) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func hasEnvPrefix(env []string, name string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, name+"=") {
			return true
		}
	}
	return false
}
