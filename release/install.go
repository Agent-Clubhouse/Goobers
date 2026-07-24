package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

const installScriptFile = "install.sh"

//go:embed install.sh
var installScript []byte

func writeInstallScript(outDir string) (string, error) {
	path := filepath.Join(outDir, installScriptFile)
	if err := os.WriteFile(path, installScript, 0o755); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
