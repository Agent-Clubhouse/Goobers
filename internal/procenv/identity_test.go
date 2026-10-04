package procenv

import (
	"strings"
	"testing"
)

func TestIsolatedIdentityEnvironmentDoesNotInheritAuthOrConfig(t *testing.T) {
	t.Setenv("GH_TOKEN", "ambient-forge-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-cloud-secret")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_VALUE_0", "credential-helper-secret")
	t.Setenv("HTTP_PROXY", "https://secret@proxy.test")
	home := t.TempDir()
	values := map[string]string{}
	for _, entry := range IsolatedIdentityEnvironment(home) {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
		if strings.Contains(value, "secret") {
			t.Fatal("ambient credential inherited")
		}
	}
	for _, key := range []string{"HOME", "USERPROFILE"} {
		if values[key] != home {
			t.Fatalf("%s did not use isolated home", key)
		}
	}
	for _, key := range []string{"GH_CONFIG_DIR", "AZURE_CONFIG_DIR", "AWS_SHARED_CREDENTIALS_FILE", "GIT_CONFIG_GLOBAL"} {
		if !strings.HasPrefix(values[key], home) {
			t.Fatalf("%s remained ambient", key)
		}
	}
	if values["GIT_TERMINAL_PROMPT"] != "0" || values["GIT_CONFIG_NOSYSTEM"] != "1" {
		t.Fatal("Git auth fallback enabled")
	}
}
