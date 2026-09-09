package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestConfigSeedCLIInitializesDispatchInputs(t *testing.T) {
	layout := instance.NewLayout(initDeterministicDemo(t))
	config, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	config.ConfigMirrorPath = t.TempDir()
	digest, err := configDirectoryDigest(layout.ConfigDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &configReloader{layout: layout, setup: &schedulerSetup{Config: config}, appliedDigest: digest}
	if err := r.publishConfigMirror(t.Context()); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "instance")
	for range 2 {
		var stdout, stderr bytes.Buffer
		code := run([]string{"config-seed", "--mirror", config.ConfigMirrorPath, "--instance", destination}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("seed exit=%d: %s", code, stderr.String())
		}
		if err := validateSeededWorkerConfig(destination); err != nil {
			t.Fatal(err)
		}
	}
}
