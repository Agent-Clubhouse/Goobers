//go:build topology_image

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/goobers/goobers/test/testsupport/testdep"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Optional runtime contract check against the prepared release base. It needs
// only a local Docker image, no cluster, network, registry auth, or publication.
func TestIntegrationPreparedTopologyInitRunsInReleaseBase(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_TOPOLOGY_TEST_IMAGE")
	testdep.Require(t, "docker")
	image := os.Getenv("GOOBERS_TOPOLOGY_TEST_IMAGE")
	o := fixture(t)
	asset := filepath.Join(o.Instance, "config", "executable.sh")
	if err := os.WriteFile(asset, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	docs := generated(t, o)
	var data map[string][]byte
	var command string
	for _, d := range docs {
		switch d["kind"] {
		case "Secret":
			var s corev1.Secret
			decode(t, d, &s)
			data = s.Data
		case "Deployment":
			var dep appsv1.Deployment
			decode(t, d, &dep)
			command = dep.Spec.Template.Spec.InitContainers[0].Args[0]
		}
	}
	bundle, prepared := t.TempDir(), t.TempDir()
	if err := os.Chmod(bundle, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(prepared, 0777); err != nil {
		t.Fatal(err)
	}
	var modes map[string]int32
	if err := json.Unmarshal(data["modes.json"], &modes); err != nil {
		t.Fatal(err)
	}
	for key, b := range data {
		mode := os.FileMode(0444)
		if modes[key]&0110 != 0 {
			mode = 0555
		}
		if err := os.WriteFile(filepath.Join(bundle, key), b, mode); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--read-only", "--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--mount", "type=bind,src="+bundle+",dst=/bundle,readonly", "--mount", "type=bind,src="+prepared+",dst=/prepared", "--entrypoint", "sh", image, "-c", command)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("release-base init failed: %v\n%s", err, output)
	}
	var index map[string]string
	if err := json.Unmarshal(data["paths.json"], &index); err != nil {
		t.Fatal(err)
	}
	for key, name := range index {
		b, err := os.ReadFile(filepath.Join(prepared, name))
		if err != nil || !bytes.Equal(b, data[key]) {
			t.Fatalf("release-base init changed or omitted %s: %v", name, err)
		}
		info, err := os.Stat(filepath.Join(prepared, name))
		if err != nil {
			t.Fatal(err)
		}
		if (info.Mode()&0111 != 0) != (modes[key]&0111 != 0) {
			t.Fatalf("executable mode changed for %s", name)
		}
	}
}
