package main

import (
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
