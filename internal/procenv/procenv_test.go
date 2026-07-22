package procenv

import (
	"strings"
	"testing"
)

func contains(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// TestBaseEnvPassesThroughGoToolchainFamily is the regression test for #248:
// a stage's `local-ci` (`make ci` -> `go build`/`go test`) must see a
// relocated Go cache/module store/proxy, not silently fall back to
// HOME-derived defaults that don't exist on a customized host.
func TestBaseEnvPassesThroughGoToolchainFamily(t *testing.T) {
	goVars := map[string]string{
		"GOPATH":     "/custom/gopath",
		"GOBIN":      "/custom/gobin",
		"GOCACHE":    "/custom/gocache",
		"GOMODCACHE": "/custom/gomodcache",
		"GOFLAGS":    "-mod=mod",
		"GOPROXY":    "https://proxy.example.internal",
		"GOSUMDB":    "off",
		"GOPRIVATE":  "example.internal/*",
	}
	for name, value := range goVars {
		t.Setenv(name, value)
	}

	env := BaseEnv()
	for name, value := range goVars {
		want := name + "=" + value
		found := false
		for _, kv := range env {
			if kv == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s did not pass through BaseEnv(), got %v", name, env)
		}
	}
}

// TestBaseEnvStillBlocksSecretShapedVars proves the Go toolchain extension
// stays default-deny — only exact allowlisted names pass.
func TestBaseEnvStillBlocksSecretShapedVars(t *testing.T) {
	t.Setenv("GOPATH_TOKEN", "should-not-pass")
	t.Setenv("MY_GOPROXY_SECRET", "should-not-pass")
	t.Setenv("GOPATH", "/custom/gopath")

	env := BaseEnv()
	for _, kv := range env {
		if kv == "GOPATH_TOKEN=should-not-pass" || kv == "MY_GOPROXY_SECRET=should-not-pass" {
			t.Fatalf("blocked var leaked into BaseEnv(): %v", env)
		}
	}
}

// TestBaseEnvPassesThroughToolchainFamilies is the #736 core: a stage running
// a non-Go toolchain (dotnet/pip/npm/cargo) against a relocated SDK root or
// cache must see those locations, not silently fall back to a HOME-derived
// default that does not exist on the host.
func TestBaseEnvPassesThroughToolchainFamilies(t *testing.T) {
	vars := map[string]string{
		"DOTNET_ROOT":           "/usr/share/dotnet",
		"DOTNET_CLI_HOME":       "/custom/dotnet-home",
		"NUGET_PACKAGES":        "/custom/nuget",
		"NUGET_HTTP_CACHE_PATH": "/custom/nuget-http",
		"VIRTUAL_ENV":           "/custom/venv",
		"PYTHONPATH":            "/custom/pymods",
		"PIP_CACHE_DIR":         "/custom/pipcache",
		"NODE_PATH":             "/custom/node_modules",
		"npm_config_cache":      "/custom/npm",
		"CARGO_HOME":            "/custom/cargo",
		"RUSTUP_HOME":           "/custom/rustup",
	}

	for name, value := range vars {
		t.Setenv(name, value)
	}
	env := BaseEnv()
	for name, value := range vars {
		if !contains(env, name+"="+value) {
			t.Fatalf("toolchain var %s did not pass through BaseEnv(), got %v", name, env)
		}
	}
}

func TestBaseEnvPassesThroughProfileLocationsWithoutAuthTokens(t *testing.T) {
	profileVars := map[string]string{
		"USERPROFILE":  `C:\Users\operator`,
		"APPDATA":      `C:\Users\operator\AppData\Roaming`,
		"LOCALAPPDATA": `C:\Users\operator\AppData\Local`,
		"HOMEDRIVE":    "C:",
		"HOMEPATH":     `\Users\operator`,
	}
	for name, value := range profileVars {
		t.Setenv(name, value)
	}
	t.Setenv("COPILOT_GITHUB_TOKEN", "must-not-pass")
	t.Setenv("GH_TOKEN", "must-not-pass")

	env := BaseEnv()
	for name, value := range profileVars {
		if !contains(env, name+"="+value) {
			t.Fatalf("profile location %s did not pass through BaseEnv(): %v", name, env)
		}
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "COPILOT_GITHUB_TOKEN=") || strings.HasPrefix(entry, "GH_TOKEN=") {
			t.Fatalf("ambient auth token leaked through profile allowlist: %v", env)
		}
	}
}

func TestBaseEnvPassesThroughWindowsRuntimeWithoutSecrets(t *testing.T) {
	runtimeVars := map[string]string{
		"SystemRoot":   `C:\Windows`,
		"WINDIR":       `C:\Windows`,
		"TEMP":         `C:\Temp`,
		"TMP":          `C:\Temp`,
		"ComSpec":      `C:\Windows\System32\cmd.exe`,
		"PATHEXT":      `.COM;.EXE;.BAT;.CMD`,
		"PSModulePath": `C:\Program Files\PowerShell\Modules`,
	}
	for name, value := range runtimeVars {
		t.Setenv(name, value)
	}
	t.Setenv("AZURE_DEVOPS_EXT_PAT", "must-not-pass")

	env := BaseEnv()
	for name, value := range runtimeVars {
		if !contains(env, name+"="+value) {
			t.Fatalf("Windows runtime var %s did not pass through BaseEnv(): %v", name, env)
		}
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "AZURE_DEVOPS_EXT_PAT=") {
			t.Fatalf("ambient token leaked through runtime allowlist: %v", env)
		}
	}
}

// TestBaseEnvExpandedAllowlistStillBlocksSecrets proves the polyglot expansion
// stays default-deny: credential-shaped ambient vars must not pass, including
// an npm per-registry setting — which is why the Node cache is allowlisted by
// its exact name (npm_config_cache), never an `npm_config_` prefix that would
// also carry `npm_config_//registry/:_authToken`.
func TestBaseEnvExpandedAllowlistStillBlocksSecrets(t *testing.T) {
	blocked := map[string]string{
		"AWS_SECRET_ACCESS_KEY": "should-not-pass",
		"NUGET_API_KEY":         "should-not-pass",
		"DOTNET_SECRET_TOKEN":   "should-not-pass",
		"npm_config_registry":   "https://secret.example.internal",
	}
	for name, value := range blocked {
		t.Setenv(name, value)
	}
	t.Setenv("DOTNET_ROOT", "/usr/share/dotnet")

	env := BaseEnv()
	for name := range blocked {
		for _, kv := range env {
			if strings.HasPrefix(kv, name+"=") {
				t.Fatalf("blocked secret-shaped var %s leaked into BaseEnv(): %v", name, env)
			}
		}
	}
	if !contains(env, "DOTNET_ROOT=/usr/share/dotnet") {
		t.Fatalf("exact allowlisted DOTNET_ROOT should still pass, got %v", env)
	}
}

// TestBaseEnvWithPassesThroughDeclaredExtra is the #736 config path: an
// instance-declared passthrough name (RunnerConfig.EnvPassthrough) is carried
// through additively on top of the built-in allowlist.
func TestBaseEnvWithPassesThroughDeclaredExtra(t *testing.T) {
	t.Setenv("MY_CUSTOM_TOOLCHAIN_HOME", "/opt/custom")
	env := BaseEnvWith([]string{"MY_CUSTOM_TOOLCHAIN_HOME"})
	if !contains(env, "MY_CUSTOM_TOOLCHAIN_HOME=/opt/custom") {
		t.Fatalf("declared extra var did not pass through: %v", env)
	}
}

// TestBaseEnvWithStaysDefaultDeny proves the config extension is an explicit
// opt-in list of names, not os.Environ() passthrough: a var not named in extra
// stays absent even when set in the daemon environment.
func TestBaseEnvWithStaysDefaultDeny(t *testing.T) {
	t.Setenv("DECLARED_VAR", "yes")
	t.Setenv("UNDECLARED_VAR", "no")
	env := BaseEnvWith([]string{"DECLARED_VAR"})
	if !contains(env, "DECLARED_VAR=yes") {
		t.Fatalf("declared var missing: %v", env)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "UNDECLARED_VAR=") {
			t.Fatalf("undeclared var leaked into BaseEnvWith(): %v", env)
		}
	}
}

// TestBaseEnvWithDeduplicates ensures a name already covered by Vars or a
// Prefixes family is emitted once, not twice, when also named in extra.
func TestBaseEnvWithDeduplicates(t *testing.T) {
	t.Setenv("GOPATH", "/custom/gopath") // already in Vars
	t.Setenv("LC_CUSTOM", "x")           // covered by the LC_ prefix family
	env := BaseEnvWith([]string{"GOPATH", "LC_CUSTOM"})
	gopath, lc := 0, 0
	for _, kv := range env {
		switch kv {
		case "GOPATH=/custom/gopath":
			gopath++
		case "LC_CUSTOM=x":
			lc++
		}
	}
	if gopath != 1 || lc != 1 {
		t.Fatalf("expected each var once, got GOPATH=%d LC_CUSTOM=%d: %v", gopath, lc, env)
	}
}

// TestBaseEnvWithNilMatchesBaseEnv locks the no-additions path to be identical
// to BaseEnv (the executor/harness drift-guard depends on it).
func TestBaseEnvWithNilMatchesBaseEnv(t *testing.T) {
	t.Setenv("GOPATH", "/custom/gopath")
	t.Setenv("LC_ALL", "C")
	if strings.Join(BaseEnvWith(nil), "\x00") != strings.Join(BaseEnv(), "\x00") {
		t.Fatalf("BaseEnvWith(nil) != BaseEnv():\n with: %v\n base: %v", BaseEnvWith(nil), BaseEnv())
	}
}

func TestValidName(t *testing.T) {
	valid := []string{"DOTNET_ROOT", "npm_config_cache", "_FOO", "A1_B2", "PATH", "x"}
	invalid := []string{"", "1FOO", "FOO=BAR", "FOO BAR", "FOO-BAR", "FOO.BAR", "npm_config_//x/:_t"}
	for _, name := range valid {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) = false, want true", name)
		}
	}
	for _, name := range invalid {
		if ValidName(name) {
			t.Errorf("ValidName(%q) = true, want false", name)
		}
	}
}
