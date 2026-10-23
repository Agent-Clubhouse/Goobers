package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

var workerExecutorBinary = resolveWorkerExecutorBinary

// The cloud executable owns worker hosting; provider stages and harness MCP
// servers run through the local CLI shipped beside it in the release image.
func resolveWorkerExecutorBinary() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve cloud executable: %w", err)
	}
	return findWorkerExecutorBinary(executable, runtime.GOOS, exec.LookPath)
}

func findWorkerExecutorBinary(executable, goos string, lookPath func(string) (string, error)) (string, error) {
	name := "goobers"
	if goos == "windows" {
		name += ".exe"
	}
	if binary, err := lookPath(filepath.Join(filepath.Dir(executable), name)); err == nil {
		return binary, nil
	}
	binary, err := lookPath(name)
	if err != nil {
		return "", fmt.Errorf("worker requires the goobers stage executable beside goobers-cloud or on PATH: %w", err)
	}
	return filepath.Abs(binary)
}
