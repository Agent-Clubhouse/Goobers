package interactiveexec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/safepath"
	"github.com/goobers/goobers/internal/sandbox"
)

func (e *Executor) nativeSandbox() (sandbox.Sandbox, error) {
	base := e.config.Sandbox
	if base == nil {
		create := e.newSandbox
		if create == nil {
			create = sandbox.New
		}
		var err error
		base, err = create()
		if err != nil {
			return nil, errors.Join(harness.ErrSandboxUnavailable, err)
		}
	}
	if base == nil || e.config.StateDir == "" {
		return nil, fmt.Errorf("%w: interactive sandbox and private server state are required", harness.ErrSandboxUnavailable)
	}
	state, err := filepath.Abs(e.config.StateDir)
	if err != nil {
		return nil, err
	}
	browserRoots, err := humanBrowserBoundaries()
	if err != nil {
		return nil, errors.Join(harness.ErrSandboxUnavailable, err)
	}
	return sandbox.WithReadBoundaries(sandbox.WithReadDenials(base, []string{state}), browserRoots), nil
}

func chatStatePath(state, workspace string) string {
	return filepath.Join(filepath.Clean(state)+"-chat-state", filepath.Base(workspace))
}

func (e *Executor) bridgeRuntimeDir() (string, error) {
	state, err := filepath.Abs(e.config.StateDir)
	if err != nil {
		return "", err
	}
	// Unix sockets have short pathname limits on Darwin. Keep this shared,
	// host-controlled root independent of the user's potentially long state path.
	key := sha256.Sum256([]byte(state))
	return safepath.MkdirLeaf("/tmp", "gbr-i-"+hex.EncodeToString(key[:10]), 0700)
}

func (e *Executor) sessionSandbox(base sandbox.Sandbox, workspace, bridgeDirectory string) (sandbox.Sandbox, error) {
	bridgeRoot, err := e.bridgeRuntimeDir()
	if err != nil {
		return nil, err
	}
	boundaries := []sandbox.ReadBoundary{{Root: bridgeRoot}}
	if bridgeDirectory != "" {
		boundaries[0].Allow = []string{bridgeDirectory}
	}
	for _, suffix := range []string{"-workspaces", "-chat-state"} {
		root := filepath.Clean(e.config.StateDir) + suffix
		created, err := safepath.MkdirLeaf(filepath.Dir(root), filepath.Base(root), 0700)
		if err != nil {
			return nil, err
		}
		own := workspace
		if suffix == "-chat-state" {
			own, err = safepath.MkdirLeaf(created, filepath.Base(workspace), 0700)
			if err != nil {
				return nil, err
			}
		}
		own, err = filepath.EvalSymlinks(own)
		if err != nil {
			return nil, err
		}
		boundary := sandbox.ReadBoundary{Root: created}
		// Native production workspaces live under this root; injected fixture
		// workspaces may be elsewhere and need no exception to the shared root.
		relative, err := filepath.Rel(created, own)
		if err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			boundary.Allow = []string{own}
		}
		boundaries = append(boundaries, boundary)
	}
	return sandbox.WithReadBoundaries(base, boundaries), nil
}
