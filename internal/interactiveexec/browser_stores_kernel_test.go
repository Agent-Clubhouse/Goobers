//go:build darwin || linux

package interactiveexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/sandbox"
)

func TestInteractiveBrowserAndHumanStoresAreKernelDenied(t *testing.T) {
	base, err := sandbox.New()
	if errors.Is(err, sandbox.ErrUnavailable) || errors.Is(err, sandbox.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	relative := ".config/chromium"
	if runtime.GOOS == "darwin" {
		relative = "Library/Application Support/Chromium"
	}
	browser := filepath.Join(home, relative)
	if err := os.MkdirAll(browser, 0700); err != nil {
		t.Fatal(err)
	}
	e, _ := bridgeExecutor(t)
	e.config.Sandbox = base
	boundary, err := e.nativeSandbox()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(browser, "new-profile", "human-binding"), filepath.Join(e.config.StateDir, "human-auth.url")}
	data, _ := json.Marshal(paths)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, self, "-test.run=^TestBrowserStoreConfinedChild$", "-test.v")
	child.Dir = workspace
	child.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + workspace, "GOOBERS_BROWSER_STORE_CHILD=1", "BROWSER_STORE_PATHS=" + string(data)}
	if err := boundary.Wrap(child, sandbox.Policy{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	// Profiles and credentials created after policy compilation are still hidden.
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("synthetic-human-secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("browser storage confinement failed: %v\n%s", err, output)
	}
}

func TestBrowserStoreConfinedChild(t *testing.T) {
	if os.Getenv("GOOBERS_BROWSER_STORE_CHILD") != "1" {
		t.Skip("subprocess fixture")
	}
	var paths []string
	if err := json.Unmarshal([]byte(os.Getenv("BROWSER_STORE_PATHS")), &paths); err != nil || len(paths) != 2 {
		t.Fatal("invalid fixture paths")
	}
	for _, path := range paths {
		if _, err := os.ReadFile(path); err == nil {
			t.Fatal("confined agent read browser or human authentication data")
		}
	}
	if err := os.WriteFile("own-output", []byte("allowed"), 0600); err != nil {
		t.Fatal("own workspace positive control failed", err)
	}
}
