package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestConfigShowYAML renders a scaffolded instance's config as YAML and checks
// the structurally-important fields round-trip.
func TestConfigShowYAML(t *testing.T) {
	root := filepath.Join(t.TempDir(), "inst")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init code = %d, stderr = %q", code, stderr)
	}
	code, stdout, stderr := runArgs(t, "config", "show", root)
	if code != 0 {
		t.Fatalf("config show code = %d, stderr = %q", code, stderr)
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("config show output is not valid YAML: %v\n%s", err, stdout)
	}
	if got["kind"] != "Instance" {
		t.Fatalf("kind = %v, want Instance", got["kind"])
	}
	if !strings.Contains(stdout, "provider: github") {
		t.Fatalf("expected the scaffolded repo in output:\n%s", stdout)
	}
}

// TestConfigShowJSON renders as JSON with --json.
func TestConfigShowJSON(t *testing.T) {
	root := filepath.Join(t.TempDir(), "inst")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init code = %d, stderr = %q", code, stderr)
	}
	code, stdout, stderr := runArgs(t, "config", "show", "--json", root)
	if code != 0 {
		t.Fatalf("config show --json code = %d, stderr = %q", code, stderr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("config show --json output is not valid JSON: %v\n%s", err, stdout)
	}
	if got["apiVersion"] == nil {
		t.Fatalf("expected apiVersion in JSON:\n%s", stdout)
	}
}

// TestConfigShowRedactsSecrets is the load-bearing safety check: even when the
// token's env var is set to a real secret, `config show` must print only the
// locator (the env var name), never resolve or leak the secret value.
func TestConfigShowRedactsSecrets(t *testing.T) {
	const secret = "ghp_THIS_MUST_NOT_APPEAR_IN_OUTPUT_0123456789"
	t.Setenv("GOOBERS_GITHUB_TOKEN", secret)

	root := filepath.Join(t.TempDir(), "inst")
	if code, _, stderr := runArgs(t, "init", root); code != 0 {
		t.Fatalf("init code = %d, stderr = %q", code, stderr)
	}
	for _, args := range [][]string{
		{"config", "show", root},
		{"config", "show", "--json", root},
	} {
		code, stdout, stderr := runArgs(t, args...)
		if code != 0 {
			t.Fatalf("%v code = %d, stderr = %q", args, code, stderr)
		}
		if strings.Contains(stdout, secret) {
			t.Fatalf("%v leaked the secret token value:\n%s", args, stdout)
		}
		if !strings.Contains(stdout, "GOOBERS_GITHUB_TOKEN") {
			t.Fatalf("%v should show the token locator (env var name):\n%s", args, stdout)
		}
	}
}

// TestConfigShowNotInstance returns usage exit code outside an instance root.
func TestConfigShowNotInstance(t *testing.T) {
	code, _, stderr := runArgs(t, "config", "show", t.TempDir())
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "not an instance root") {
		t.Fatalf("stderr = %q, want an instance-root hint", stderr)
	}
}

// TestConfigBareUsage prints group usage and a non-zero exit with no subcommand.
func TestConfigBareUsage(t *testing.T) {
	code, _, stderr := runArgs(t, "config")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "config show") {
		t.Fatalf("stderr = %q, want the show subcommand listed", stderr)
	}
}
