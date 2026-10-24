package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestDeployReferenceCloudKeepsSharedKeyAuthInKindOverlay(t *testing.T) {
	production, err := os.ReadFile(filepath.Join(cloudRefDir, "workload/control-deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := os.ReadFile("../../test/ha/kind/cloud-reference/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--object-store-account-name", "--object-store-account-key-file"} {
		if strings.Contains(string(production), flag) {
			t.Errorf("production control Deployment selects shared-key auth with %s", flag)
		}
		if !strings.Contains(string(overlay), flag) {
			t.Errorf("kind overlay does not opt into shared-key auth with %s", flag)
		}
	}
}

func TestDeployReferenceCloudMountedConfigLoadsAsInstanceTree(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(filepath.Join(configDir, "gaggles", "reference"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFixture := func(from, to string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(cloudRefDir, "prereqs", "config", from))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	copyFixture("manifest.yaml", filepath.Join(configDir, "manifest.yaml"))
	copyFixture("gaggle.yaml", filepath.Join(configDir, "gaggles", "reference", "gaggle.yaml"))
	set, report, err := instance.LoadConfigDir(configDir)
	if err != nil || report == nil || report.HasErrors() || set == nil || len(set.Gaggles) != 1 {
		t.Fatalf("mounted reference config did not load: set=%+v report=%+v err=%v", set, report, err)
	}
}
