package main

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReferenceBicepCompilesWithOfflineCLI(t *testing.T) {
	t.Parallel()
	// offlineBicepCLI fails instead of skipping when GOOBERS_REQUIRE_BICEP is
	// set, as CI's preflight job does after provisioning the pinned CLI.
	binary := offlineBicepCLI(t)
	root := moduleRoot(t)
	command := exec.Command(binary, "build", filepath.Join(root, "infra", "bicep", "main.bicep"), "--stdout")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile reference Bicep: %v\n%s", err, output)
	}
}
