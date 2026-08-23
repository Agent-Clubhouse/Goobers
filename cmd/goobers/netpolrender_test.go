package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/runnercap"
)

// netpolTestMeta is the fake upstream provenance document the fixture's
// sourceSHA256 markers are computed from.
var netpolTestMeta = []byte(`{"api":["140.82.112.0/20"],"copilot":["140.82.113.21/32"]}`)

func netpolTestMetaSHA() string {
	return fmt.Sprintf("%x", sha256.Sum256(netpolTestMeta))
}

func writeNetpolInstance(t *testing.T, egressExtra string) string {
	t.Helper()
	root := t.TempDir()
	yaml := `apiVersion: goobers.dev/v1alpha1
kind: Instance
schemaVersion: 2
repos: []
runners:
  - name: self
    host: self
  - name: ci-linux
    host: ghcr.io/example/goobers-ci:v0.7.0
    provides:
      os: linux
    restrictions: [network:allowlist, tmp:ephemeral]
  - name: locked
    host: ghcr.io/example/goobers-locked:v0.7.0
    provides:
      os: linux
    restrictions: [network:none]
engine:
  hostPort: temporal.goobers-system:7233
egress:
  allowlist:
    - name: github-provider
      kind: provider
      source: https://api.github.com/meta
      sourceSHA256: ` + netpolTestMetaSHA() + `
      cidrs: [140.82.112.0/20]
    - name: copilot-model
      kind: model
      source: https://api.github.com/meta
      sourceSHA256: ` + netpolTestMetaSHA() + `
      cidrs: [140.82.113.21/32]
` + egressExtra
	if err := os.WriteFile(filepath.Join(root, "instance.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func stubNetpolFetch(t *testing.T, body []byte) {
	t.Helper()
	previous := netpolRenderFetch
	netpolRenderFetch = func(ctx context.Context, url string) ([]byte, error) {
		return body, nil
	}
	t.Cleanup(func() { netpolRenderFetch = previous })
}

func TestNetpolRenderWritesPerClassManifests(t *testing.T) {
	root := writeNetpolInstance(t, "")
	out := filepath.Join(t.TempDir(), "netpol")
	var stdout, stderr bytes.Buffer
	if code := runNetpolRender([]string{"--out", out, root}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}

	allowClass := runnercap.RunnerClassValue([]string{"network:allowlist", "tmp:ephemeral"})
	noneClass := runnercap.RunnerClassValue([]string{"network:none"})
	for _, name := range []string{"netpol-" + allowClass + ".yaml", "netpol-" + noneClass + ".yaml", "kustomization.yaml"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("expected rendered file %s: %v", name, err)
		}
	}
	// The self runner renders no class.
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("rendered %d files, want 3 (two classes + kustomization)", len(entries))
	}
}

// Zero-declaration invariance at the command boundary: an instance with no
// runners: block (implicit self) renders no per-class policies and exits 0.
func TestNetpolRenderZeroDeclarationInvariance(t *testing.T) {
	root := t.TempDir()
	yaml := `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos: []
`
	if err := os.WriteFile(filepath.Join(root, "instance.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runNetpolRender([]string{root}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "nothing to render") {
		t.Errorf("stdout = %q, want the nothing-to-render notice", stdout.String())
	}
}

// The render refuses placeholders at the command boundary too: an egress
// group still holding a documentation CIDR fails, no stub is written.
func TestNetpolRenderRefusesPlaceholderConfig(t *testing.T) {
	root := writeNetpolInstance(t, `    - name: sandbox-placeholder
      kind: sandbox
      cidrs: [192.0.2.0/24]
`)
	out := filepath.Join(t.TempDir(), "netpol")
	var stdout, stderr bytes.Buffer
	if code := runNetpolRender([]string{"--out", out, root}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "documentation range") {
		t.Errorf("stderr = %q, want documentation-range refusal", stderr.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("a refused render still wrote output")
	}
}

func TestNetpolRenderCheckLifecycle(t *testing.T) {
	root := writeNetpolInstance(t, "")
	out := filepath.Join(t.TempDir(), "netpol")
	stubNetpolFetch(t, netpolTestMeta)

	// Render + freeze the baseline.
	var stdout, stderr bytes.Buffer
	if code := runNetpolRender([]string{"--out", out, "--write-baseline", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("render+freeze exit %d, stderr: %s", code, stderr.String())
	}

	// A clean check passes.
	stdout.Reset()
	stderr.Reset()
	if code := runNetpolRender([]string{"--out", out, "--check", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("clean --check exit %d, stderr: %s", code, stderr.String())
	}

	// Upstream rotates: EVERY marker is stale, and --check fails naming the
	// drift.
	stubNetpolFetch(t, []byte(`{"api":["140.82.112.0/20","4.148.0.0/16"]}`))
	stdout.Reset()
	stderr.Reset()
	if code := runNetpolRender([]string{"--out", out, "--check", root}, &stdout, &stderr); code != 1 {
		t.Fatalf("rotated-upstream --check exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "rotated") {
		t.Errorf("stderr = %q, want rotation drift", stderr.String())
	}
	// Both groups share the URL and both markers went stale — both named,
	// never just the first.
	for _, group := range []string{"github-provider", "copilot-model"} {
		if !strings.Contains(stderr.String(), group) {
			t.Errorf("stderr does not name stale group %q — a first-only check", group)
		}
	}
}

// A missing baseline must fail --check, never silently pass.
func TestNetpolRenderCheckRefusesMissingBaseline(t *testing.T) {
	root := writeNetpolInstance(t, "")
	out := filepath.Join(t.TempDir(), "netpol")
	stubNetpolFetch(t, netpolTestMeta)
	var stdout, stderr bytes.Buffer
	if code := runNetpolRender([]string{"--out", out, root}, &stdout, &stderr); code != 0 {
		t.Fatalf("render exit %d, stderr: %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runNetpolRender([]string{"--out", out, "--check", root}, &stdout, &stderr); code != 1 {
		t.Fatalf("baseline-less --check exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "never passes silently") {
		t.Errorf("stderr = %q, want the missing-baseline refusal", stderr.String())
	}
}

// The coverage ratchet fails on a RISE: widen the model group past the frozen
// baseline and --check reports the class in addresses.
func TestNetpolRenderCheckFailsOnCoverageRise(t *testing.T) {
	root := writeNetpolInstance(t, "")
	out := filepath.Join(t.TempDir(), "netpol")
	stubNetpolFetch(t, netpolTestMeta)
	var stdout, stderr bytes.Buffer
	if code := runNetpolRender([]string{"--out", out, "--write-baseline", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("freeze exit %d, stderr: %s", code, stderr.String())
	}

	// The model endpoint set widens (aggregate rotation): same file, wider
	// CIDR. Re-render so the manifests match, then check against the OLD
	// baseline.
	wider := writeNetpolInstance(t, "")
	raw, err := os.ReadFile(filepath.Join(wider, "instance.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	widened := strings.Replace(string(raw), "cidrs: [140.82.113.21/32]", "cidrs: [140.82.113.0/24]", 1)
	if err := os.WriteFile(filepath.Join(root, "instance.yaml"), []byte(widened), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runNetpolRender([]string{"--out", out, root}, &stdout, &stderr); code != 0 {
		t.Fatalf("re-render exit %d, stderr: %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := runNetpolRender([]string{"--out", out, "--check", root}, &stdout, &stderr); code != 1 {
		t.Fatalf("risen-coverage --check exit %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ROSE") || !strings.Contains(stderr.String(), "addresses") {
		t.Errorf("stderr = %q, want an address-unit rise failure", stderr.String())
	}
}

// --check with --out also refuses stale on-disk output.
func TestNetpolRenderCheckRefusesStaleOutput(t *testing.T) {
	root := writeNetpolInstance(t, "")
	out := filepath.Join(t.TempDir(), "netpol")
	stubNetpolFetch(t, netpolTestMeta)
	var stdout, stderr bytes.Buffer
	if code := runNetpolRender([]string{"--out", out, "--write-baseline", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("render exit %d, stderr: %s", code, stderr.String())
	}
	// Hand-edit one rendered file — decision 016 says nobody hand-edits the
	// rendered copy; --check catches it.
	allowClass := runnercap.RunnerClassValue([]string{"network:allowlist", "tmp:ephemeral"})
	path := filepath.Join(out, "netpol-"+allowClass+".yaml")
	if err := os.WriteFile(path, []byte("# hand-edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runNetpolRender([]string{"--out", out, "--check", root}, &stdout, &stderr); code != 1 {
		t.Fatalf("stale-output --check exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "stale output") {
		t.Errorf("stderr = %q, want stale-output refusal", stderr.String())
	}
}
