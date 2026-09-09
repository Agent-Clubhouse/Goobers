package main

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/configmirror"
	"github.com/goobers/goobers/internal/instance"
)

func TestConfigMirrorPublishesOnlyAppliedGeneration(t *testing.T) {
	root := initDeterministicDemo(t)
	layout := instance.NewLayout(root)
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	config.ConfigMirrorPath = t.TempDir()
	config.WorkflowSource = &instance.WorkflowSource{Kind: instance.WorkflowSourceKindGit}
	digest, err := configDirectoryDigest(layout.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &configReloader{layout: layout, setup: &schedulerSetup{Config: config}, appliedDigest: digest}
	if err := r.publishConfigMirror(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := configmirror.Open(config.ConfigMirrorPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	dest := t.TempDir()
	if err := snapshot.Extract(t.Context(), dest); err != nil {
		t.Fatal(err)
	}
	document, err := os.ReadFile(filepath.Join(dest, "instance.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var worker instance.Config
	if err := yaml.UnmarshalStrict(document, &worker); err != nil {
		t.Fatal(err)
	}
	if worker.WorkflowSource != nil || worker.ConfigMirrorPath != "" {
		t.Fatal("worker retained daemon source or mirror configuration")
	}
	if config.WorkflowSource == nil || config.ConfigMirrorPath == "" {
		t.Fatal("publication mutated daemon configuration")
	}
	public := filepath.Join(config.ConfigMirrorPath, configmirror.SnapshotName)
	before, err := os.ReadFile(public)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a new applied identity while the source still contains another
	// generation. It must not be acknowledged or replace the previous archive.
	r.appliedDigest = "not-the-captured-generation"
	if err := r.publishConfigMirror(t.Context()); err == nil {
		t.Fatal("published mismatched generation")
	}
	after, err := os.ReadFile(public)
	if err != nil || string(before) != string(after) || r.mirroredDigest != digest {
		t.Fatalf("failed publication replaced the accepted snapshot: %v", err)
	}
}

func TestConfigMirrorAbsentSettingDoesNothing(t *testing.T) {
	r := &configReloader{setup: &schedulerSetup{Config: &instance.Config{}}}
	if err := r.publishConfigMirror(t.Context()); err != nil {
		t.Fatal(err)
	}
	stop := r.startConfigMirror(t.Context())
	stop()
}

func TestConfigMirrorRetriesUnchangedAppliedDigestAfterShareFailure(t *testing.T) {
	layout := instance.NewLayout(initDeterministicDemo(t))
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	config.ConfigMirrorPath = filepath.Join(t.TempDir(), "share")
	if err := os.WriteFile(config.ConfigMirrorPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := configDirectoryDigest(layout.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &configReloader{layout: layout, setup: &schedulerSetup{Config: config}, appliedDigest: digest}
	r.refreshConfigMirror(t.Context())
	if r.mirroredDigest != "" || r.lastMirrorError == "" {
		t.Fatal("unavailable share was acknowledged or not surfaced")
	}
	if err := os.Remove(config.ConfigMirrorPath); err != nil {
		t.Fatal(err)
	}
	r.refreshConfigMirror(t.Context())
	if r.mirroredDigest != digest || r.lastMirrorError != "" {
		t.Fatalf("unchanged generation did not recover: %s", r.lastMirrorError)
	}
}
