package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReferenceBicepCompilesWithOfflineCLI(t *testing.T) {
	t.Parallel()
	binary, err := exec.LookPath("bicep")
	if err != nil {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			t.Skip("Bicep CLI is not installed")
		}
		binary = filepath.Join(home, ".azure", "bin", "bicep")
		if info, statErr := os.Stat(binary); statErr != nil || info.IsDir() {
			t.Skip("Bicep CLI is not installed offline")
		}
	}
	root := moduleRoot(t)
	command := exec.Command(binary, "build", filepath.Join(root, "infra", "bicep", "main.bicep"), "--stdout")
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile reference Bicep: %v\n%s", err, output)
	}
}
