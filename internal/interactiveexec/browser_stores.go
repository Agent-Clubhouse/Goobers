package interactiveexec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/goobers/goobers/internal/sandbox"
)

// Browser storage can hold the human control credential. These masks apply to
// interactive invocations only and never inspect browser data. Custom profiles
// outside these roots require explicit host confinement; this is not a complete
// same-UID boundary against browser IPC or host process memory.
func humanBrowserBoundaries() ([]sandbox.ReadBoundary, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("interactive browser storage confinement: %w", err)
	}
	return browserBoundaries(home, runtime.GOOS)
}

func browserBoundaries(home, platform string) ([]sandbox.ReadBoundary, error) {
	var roots []string
	switch platform {
	case "darwin":
		roots = []string{
			"Library/Application Support/Google/Chrome",
			"Library/Application Support/Chromium",
			"Library/Application Support/Microsoft Edge",
			"Library/Application Support/Firefox",
			"Library/Cookies", "Library/Safari",
			"Library/Containers/com.apple.Safari",
			"Library/Group Containers/group.com.apple.Safari",
			"Library/Keychains",
		}
	case "linux":
		roots = []string{
			".config/google-chrome", ".config/chromium", ".config/microsoft-edge",
			".mozilla", ".local/share/keyrings",
		}
	}
	var boundaries []sandbox.ReadBoundary
	for _, relative := range roots {
		path := filepath.Join(home, filepath.FromSlash(relative))
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue // Newly installed browsers require a new invocation.
		}
		if err != nil {
			return nil, fmt.Errorf("interactive browser storage cannot be inspected: %w", err)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("interactive browser storage cannot be resolved: %w", err)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("interactive browser storage is not a directory")
		}
		boundaries = addBrowserBoundary(boundaries, resolved)
	}
	return boundaries, nil
}

func beneathBrowserRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func addBrowserBoundary(boundaries []sandbox.ReadBoundary, path string) []sandbox.ReadBoundary {
	for _, existing := range boundaries {
		if beneathBrowserRoot(existing.Root, path) {
			return boundaries
		}
	}
	kept := boundaries[:0]
	for _, existing := range boundaries {
		if !beneathBrowserRoot(path, existing.Root) {
			kept = append(kept, existing)
		}
	}
	return append(kept, sandbox.ReadBoundary{Root: path})
}
