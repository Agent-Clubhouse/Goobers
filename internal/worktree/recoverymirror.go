package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WithRecoveryMirror visits a managed mirror under the repository lock, creating
// an empty full mirror when necessary. It never fetches or resolves credentials:
// recovery bundles are self-contained and may arrive while the forge is down.
// The normal WorkingCopy path can subsequently refresh this mirror. The caller
// must authorize the exact configured URL before invoking this method.
func (m *Manager) WithRecoveryMirror(ctx context.Context, repoURL string, visit func(string) error) error {
	if repoURL == "" || visit == nil {
		return fmt.Errorf("recovery mirror requires repository identity and visitor")
	}
	key := repoKey(repoURL)
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	parent := filepath.Join(m.Root, key)
	for _, path := range []string{m.Root, parent} {
		if err := ensureRecoveryMirrorDirectory(path); err != nil {
			return err
		}
	}
	dir := m.repoDirForKey(key)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := initializeRecoveryMirror(ctx, parent, dir, repoURL); err != nil {
			return err
		}
	} else if err != nil || !info.IsDir() {
		return fmt.Errorf("recovery mirror requires a real repository directory")
	}
	return visit(dir)
}

func ensureRecoveryMirrorDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("recovery mirror requires real managed directories")
	}
	return fsyncDir(filepath.Dir(path))
}

func initializeRecoveryMirror(ctx context.Context, parent, destination, repoURL string) error {
	// One deterministic staging directory per configured repository bounds
	// interrupted initialization. Retry reuses it; no random crash debris grows.
	staging := filepath.Join(parent, "recovery-mirror-init")
	if err := ensureRecoveryMirrorDirectory(staging); err != nil {
		return err
	}
	commands := [][]string{
		{"init", "--bare", "--template=", "--initial-branch=main", "--object-format=sha1", "."},
		{"config", "--local", "remote.origin.url", repoURL},
		{"config", "--local", "remote.origin.fetch", "+refs/*:refs/*"},
		{"config", "--local", "remote.origin.mirror", "true"},
		{"config", "--local", "core.autocrlf", "false"},
		{"config", "--local", "core.longpaths", "true"},
	}
	for _, args := range commands {
		if _, err := rawGitOutput(ctx, staging, recoveryMirrorEnvironment(), args...); err != nil {
			return fmt.Errorf("initialize recovery mirror: %w", err)
		}
	}
	// Persist configuration before publishing the directory. The archive intake
	// separately makes imported objects and refs durable before acknowledging.
	for _, name := range []string{"HEAD", "config"} {
		file, err := os.Open(filepath.Join(staging, name))
		if err != nil {
			return err
		}
		if err := errors.Join(file.Sync(), file.Close()); err != nil {
			return err
		}
	}
	if err := fsyncDir(staging); err != nil {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		return err
	}
	return fsyncDir(parent)
}

func recoveryMirrorEnvironment() []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}
