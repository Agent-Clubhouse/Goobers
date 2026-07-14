package executor

import (
	"strings"
	"testing"
)

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

	env := baseEnv()
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

	env := baseEnv()
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
