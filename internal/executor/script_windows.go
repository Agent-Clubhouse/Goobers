//go:build windows

package executor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const inlineScriptPathEnv = "GOOBERS_INLINE_SCRIPT_PATH"

func scriptCommand(script string) ([]string, []string, func(), error) {
	file, err := os.CreateTemp("", "goobers-inline-*.cmd")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("executor: create inline script: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	script = strings.ReplaceAll(script, "\r\n", "\n")
	script = strings.ReplaceAll(script, "\n", "\r\n")
	if _, err := file.WriteString(script); err != nil {
		_ = file.Close()
		cleanup()
		return nil, nil, nil, fmt.Errorf("executor: write inline script: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return nil, nil, nil, fmt.Errorf("executor: close inline script: %w", err)
	}
	// cmd.exe does not follow CommandLineToArgvW quoting. Expand a quoted
	// environment value so scripts and paths with quotes never enter argv.
	command := []string{"cmd.exe", "/D", "/S", "/C", "%" + inlineScriptPathEnv + "%"}
	env := []string{inlineScriptPathEnv + "=\"" + path + "\""}
	return command, env, cleanup, nil
}

// commandInvocation resolves how a deterministic stage's declared command
// (e.g. ["npm", "run", "ci"]) actually gets launched on Windows. Go's
// os/exec *can* locate and start a .bat/.cmd batch file directly — LookPath
// finds it via PATHEXT, and exec.Command accepts the resulting path — but
// stock npm installs on Windows only ship "npm.cmd" (a batch shim; the
// project's own merge-gate orchestrator already had to work around exactly
// this by wrapping every npm invocation through cmd.exe rather than starting
// npm.cmd directly, see test/ci/main.go's commandInvocation from #1084). A
// local-ci stage running ["npm", "run", "ci"] hit the same failure mode: it
// resolves to npm.cmd and starting it directly does not reliably behave like
// running it from a real command prompt. Route through cmd.exe /d (skip
// AutoRun) /s (preserve the rest of the line's quoting) /c the same way, so
// cmd.exe does its own PATH/PATHEXT resolution and interpretation instead of
// Go trying to start a batch file as if it were a normal PE executable. Real
// executables (.exe/.com, e.g. "make" via choco, "go") are left untouched —
// exec.Command already starts those correctly.
func commandInvocation(name string, args []string) (string, []string) {
	resolved, err := exec.LookPath(name)
	if err != nil || !isBatchFile(resolved) {
		return name, args
	}
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	wrapped := make([]string, 0, len(args)+4)
	wrapped = append(wrapped, "/d", "/s", "/c", name)
	wrapped = append(wrapped, args...)
	return comspec, wrapped
}

func isBatchFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".bat", ".cmd":
		return true
	default:
		return false
	}
}
