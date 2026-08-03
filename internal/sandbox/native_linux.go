//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const bubblewrapPreflightTimeout = 5 * time.Second

type nativeSandbox struct {
	bubblewrapPath string
}

func newNative() (Sandbox, error) {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("%w: bubblewrap (bwrap) not found on PATH", ErrUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), bubblewrapPreflightTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, path,
		"--die-with-parent",
		"--unshare-pid",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--", "/bin/true",
	).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			return nil, fmt.Errorf("%w: bubblewrap preflight: %w: %s", ErrUnavailable, err, detail)
		}
		return nil, fmt.Errorf("%w: bubblewrap preflight: %w", ErrUnavailable, err)
	}
	return nativeSandbox{bubblewrapPath: path}, nil
}

func (nativeSandbox) Mechanism() string { return "bwrap" }

func (s nativeSandbox) Wrap(command *exec.Cmd, policy Policy) error {
	validated, err := validate(command, policy)
	if err != nil {
		return err
	}
	targetPath := command.Path
	targetArgs := append([]string(nil), command.Args[1:]...)
	command.Path = s.bubblewrapPath
	args := []string{
		"bwrap",
		"--die-with-parent",
		"--unshare-pid",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}
	for _, root := range outermostRoots(validated.privateRoots) {
		args = append(args, "--tmpfs", root)
	}
	for _, path := range privateCommandPaths(targetPath, validated.privateRoots) {
		args = appendParentDirs(args, path, validated.privateRoots)
		args = append(args, "--ro-bind", path, path)
	}
	for _, root := range append([]string{validated.workspace}, validated.writableRoots...) {
		args = appendParentDirs(args, root, validated.privateRoots)
		args = append(args, "--bind", root, root)
	}
	args = append(args, "--chdir", command.Dir, "--", targetPath)
	command.Args = append(args, targetArgs...)
	return nil
}

func outermostRoots(roots []string) []string {
	var out []string
	for _, root := range roots {
		nested := false
		for _, other := range roots {
			if root != other && pathWithin(other, root) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, root)
		}
	}
	return out
}

func appendParentDirs(args []string, path string, privateRoots []string) []string {
	for _, root := range outermostRoots(privateRoots) {
		if !pathWithin(root, path) {
			continue
		}
		relative, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil || relative == "." {
			return args
		}
		current := root
		for _, part := range strings.Split(relative, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			args = append(args, "--dir", current)
		}
		return args
	}
	return args
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
