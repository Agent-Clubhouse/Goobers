//go:build linux

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestNativeSandboxIsolatesHostProc(t *testing.T) {
	s := requiredNativeSandbox(t)
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "proc-escape.txt")

	helper := exec.Command("sleep", "30")
	if err := helper.Start(); err != nil {
		t.Fatalf("start host helper: %v", err)
	}
	t.Cleanup(func() {
		if err := helper.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("kill host helper: %v", err)
		}
		_ = helper.Wait()
	})

	procRoot := filepath.Join(
		"/proc",
		strconv.Itoa(helper.Process.Pid),
		"root",
	)
	if _, err := os.Stat(procRoot); err != nil {
		t.Fatalf("stat host helper root: %v", err)
	}
	procEscape := filepath.Join(
		procRoot,
		strings.TrimPrefix(outside, string(filepath.Separator)),
	)
	command := exec.Command(
		"sh", "-c",
		`if printf 'escape' > "$1"; then exit 91; fi`,
		"sandbox-proc-isolation", procEscape,
	)
	command.Dir = workspace
	if err := s.Wrap(command, Policy{Workspace: workspace}); err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed command: %v\n%s", err, output)
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host /proc escape write was not denied: %v", err)
	}
}
