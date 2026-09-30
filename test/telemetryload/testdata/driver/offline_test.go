package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestLoadFixtureCannotAcquireGitHubAccess(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "internal", "instance", "demo", "gaggles", "demo", "workflows", "demo.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkOfflineDemoFixture(&instance.Config{}, raw); err != nil {
		t.Fatalf("bundled demo is no longer offline: %v", err)
	}
	withRepo := &instance.Config{Repos: []instance.RepoRef{{}}}
	if err := checkOfflineDemoFixture(withRepo, raw); err == nil {
		t.Fatal("repository connection was accepted")
	}
	for name, changed := range map[string]string{
		"network": strings.Replace(string(raw), "network: none", "network: host", 1),
		"command": strings.Replace(string(raw), "- __demo-provider", "- provider-api", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkOfflineDemoFixture(&instance.Config{}, []byte(changed)); err == nil {
				t.Fatal("networked or non-demo stage was accepted")
			}
		})
	}
}

func TestNeedsInsecureDemoFallback(t *testing.T) {
	failure := errors.New("exit status 2")
	restricted := []byte("error: --demo requires unprivileged user namespaces for enforced network isolation")
	tests := []struct {
		name    string
		goos    string
		allowed bool
		err     error
		output  []byte
		want    bool
	}{
		{name: "explicit Linux fallback", goos: "linux", allowed: true, err: failure, output: restricted, want: true},
		{name: "fallback not opted in", goos: "linux", err: failure, output: restricted},
		{name: "unrelated Linux error", goos: "linux", allowed: true, err: failure, output: []byte("permission denied")},
		{name: "successful Linux init", goos: "linux", allowed: true, output: restricted},
		{name: "Windows is handled directly", goos: "windows", allowed: true, err: failure, output: restricted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsInsecureDemoFallback(tt.goos, tt.allowed, tt.err, tt.output); got != tt.want {
				t.Fatalf("needsInsecureDemoFallback() = %v, want %v", got, tt.want)
			}
		})
	}
}
