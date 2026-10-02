package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/netpolrender"
)

// This name deliberately joins make deploy-validate's TestDeployReference
// selection. Check committed output directly: generating it before checking
// would silently bless renderer/config drift instead of gating it.
func TestDeployReferenceNetpolCommittedDriftGate(t *testing.T) {
	root := filepath.Join("..", "..", "deploy", "reference", "examples", "netpol-drift")
	meta, err := os.ReadFile(filepath.Join(root, "upstream-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	previous := netpolRenderFetch
	netpolRenderFetch = func(_ context.Context, url string) ([]byte, error) {
		if url != "https://api.github.com/meta" {
			return nil, fmt.Errorf("unrecorded provenance source %q", url)
		}
		return meta, nil
	}
	t.Cleanup(func() { netpolRenderFetch = previous })
	checkReferenceNetpol(t, root, "")

	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string)
		want   string
	}{
		{"inventory drift", func(t *testing.T, root string) {
			replaceReferenceNetpol(t, filepath.Join(root, "instance.yaml"), "name: ci-linux", "name: renamed-build")
		}, "stale output:"},
		{"rendered policy drift", func(t *testing.T, root string) {
			replaceReferenceNetpol(t, filepath.Join(root, "rendered", "netpol-netallow-tmpeph.yaml"), "140.82.112.0/20", "140.82.112.0/21")
		}, "stale output:"},
		{"coverage baseline drift", func(t *testing.T, root string) {
			path := filepath.Join(root, "rendered", "coverage-baseline.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			base, err := netpolrender.ParseBaseline(raw)
			if err != nil {
				t.Fatal(err)
			}
			entry := base.Classes["netallow-tmpeph"]
			entry.ModelEndpointAddresses = "0"
			base.Classes["netallow-tmpeph"] = entry
			raw, err = json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "coverage ROSE:"},
		{"missing baseline", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "rendered", "coverage-baseline.json")); err != nil {
				t.Fatal(err)
			}
		}, "cannot read baseline"},
		{"second provenance marker drift", func(t *testing.T, root string) {
			path := filepath.Join(root, "instance.yaml")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(raw), "\n")
			for i := len(lines) - 1; i >= 0; i-- {
				if strings.Contains(lines[i], "sourceSHA256:") {
					lines[i] = "      sourceSHA256: " + strings.Repeat("a", 64)
					break
				}
			}
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "provenance drift"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := t.TempDir()
			if err := os.CopyFS(copy, os.DirFS(root)); err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, copy)
			checkReferenceNetpol(t, copy, tc.want)
		})
	}
}

func checkReferenceNetpol(t *testing.T, root, wantFailure string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runNetpolRender([]string{"--check", "--out", filepath.Join(root, "rendered"), root}, &stdout, &stderr)
	if wantFailure == "" {
		if code != 0 || !strings.Contains(stdout.String(), "netpol-render --check: OK") {
			t.Fatalf("committed netpol check failed (%d): %s%s", code, stdout.String(), stderr.String())
		}
	} else if code != 1 || !strings.Contains(stderr.String(), wantFailure) {
		t.Fatalf("check = %d, want drift failure %q: %s%s", code, wantFailure, stdout.String(), stderr.String())
	}
}

func replaceReferenceNetpol(t *testing.T, path, old, replacement string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(old)) {
		t.Fatalf("fixture %s has no %q", path, old)
	}
	if err := os.WriteFile(path, bytes.ReplaceAll(raw, []byte(old), []byte(replacement)), 0o644); err != nil {
		t.Fatal(err)
	}
}
