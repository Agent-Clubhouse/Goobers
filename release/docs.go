package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const releaseDocsVersionFile = "docs/RELEASE.md"

const (
	readmeSourceInstall = "Install an exact tagged release on Linux or macOS and let its guided flow create\n" +
		"and validate a release-pinned instance:\n\n" +
		"```sh\n" +
		"VERSION=v1.2.3 # replace with the exact release to install\n" +
		"/bin/sh -c \"$(curl -fsSL \"https://github.com/Agent-Clubhouse/Goobers/releases/download/${VERSION}/install.sh\")\" \\\n" +
		"  -- \"$VERSION\" ./my-instance\n\n"
	quickstartSourceBuild = "## 1. Build the binary\n\n```sh\n" +
		"go build -o bin/goobers ./cmd/goobers    # or: make build\n```\n\n"
	linuxQuickstartSourceBuild = "## 2. Build the binary\n\n```sh\n" +
		"go build -o bin/goobers ./cmd/goobers    # or: make build\n" +
		"sudo install -m 0755 bin/goobers /usr/local/bin/goobers   # optional: put it on PATH\n```\n\n"
)

func stageReleaseDocs(version, commit, ldflags string) (string, func(), error) {
	repoRoot := gitOutput("rev-parse", "--show-toplevel")
	if repoRoot == "" {
		return "", nil, fmt.Errorf("resolve repository root for release documentation")
	}

	workDir, err := os.MkdirTemp("", "goobers-release-docs-")
	if err != nil {
		return "", nil, fmt.Errorf("create release docs workspace: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(workDir) }
	payloadDir := filepath.Join(workDir, "payload")
	docsDir := filepath.Join(payloadDir, "docs")

	if err := copyReleaseTree(filepath.Join(repoRoot, "docs"), docsDir); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := copyReleaseFile(filepath.Join(repoRoot, "README.md"), filepath.Join(payloadDir, "README.md")); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := adaptInstalledOnboarding(payloadDir, version); err != nil {
		cleanup()
		return "", nil, err
	}

	generator := filepath.Join(workDir, "goobers-docs")
	if runtime.GOOS == "windows" {
		generator += ".exe"
	}
	build := exec.Command(
		"go", "build", "-trimpath", "-ldflags", ldflags,
		"-o", generator, "./cmd/goobers",
	)
	build.Dir = repoRoot
	build.Env = append(os.Environ(),
		"GOOS="+runtime.GOOS,
		"GOARCH="+runtime.GOARCH,
		"CGO_ENABLED=0",
	)
	if output, err := build.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("build release docs generator: %w\n%s", err, output)
	}

	generate := exec.Command(generator, "__generate-docs", docsDir)
	if output, err := generate.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("generate release CLI docs: %w\n%s", err, output)
	}

	marker := fmt.Sprintf(
		"# Goobers %s documentation\n\n"+
			"This documentation tree and the sibling `goobers` binary were packaged from commit `%s`.\n"+
			"The CLI reference, man pages, and completion scripts were regenerated from that binary's command registry.\n",
		version,
		commit,
	)
	if err := os.WriteFile(filepath.Join(payloadDir, filepath.FromSlash(releaseDocsVersionFile)), []byte(marker), 0o644); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write release docs identity: %w", err)
	}
	return payloadDir, cleanup, nil
}

func adaptInstalledOnboarding(payloadDir, version string) error {
	rewrites := []struct {
		path                 string
		sourceSection        string
		installedSection     string
		sourceCommandPrefix  string
		installedCommandName string
	}{
		{
			path:          "README.md",
			sourceSection: readmeSourceInstall,
			installedSection: fmt.Sprintf(
				"This copy is bundled with release `%s` and assumes `goobers` is installed on `PATH`.\n\n"+
					"```sh\ngoobers --version\n"+
					"goobers init --guided ./my-instance\n\n",
				version,
			),
			sourceCommandPrefix:  "$HOME/.local/bin/goobers",
			installedCommandName: "goobers",
		},
		{
			path:          "docs/guides/quickstart.md",
			sourceSection: quickstartSourceBuild,
			installedSection: fmt.Sprintf(
				"## 1. Confirm the installed binary\n\n"+
					"This copy is bundled with release `%s` and uses the `goobers` executable from `PATH`.\n\n"+
					"```sh\ngoobers --version\n```\n\n",
				version,
			),
			sourceCommandPrefix:  "bin/goobers",
			installedCommandName: "goobers",
		},
		{
			path:          "docs/guides/quickstart-linux.md",
			sourceSection: linuxQuickstartSourceBuild,
			installedSection: fmt.Sprintf(
				"## 2. Confirm the installed binary\n\n"+
					"This copy is bundled with release `%s`; install the archive's `goobers` executable on `PATH`, then confirm it:\n\n"+
					"```sh\ngoobers --version\n```\n\n",
				version,
			),
		},
	}

	for _, rewrite := range rewrites {
		path := filepath.Join(payloadDir, filepath.FromSlash(rewrite.path))
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read release onboarding doc %s: %w", rewrite.path, err)
		}
		content := string(data)
		if strings.Count(content, rewrite.sourceSection) != 1 {
			return fmt.Errorf("release onboarding source section drifted in %s", rewrite.path)
		}
		content = strings.Replace(content, rewrite.sourceSection, rewrite.installedSection, 1)
		if rewrite.sourceCommandPrefix != "" {
			content = strings.ReplaceAll(content, rewrite.sourceCommandPrefix, rewrite.installedCommandName)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write release onboarding doc %s: %w", rewrite.path, err)
		}
	}
	return nil
}

func copyReleaseTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create release docs directory %s: %w", target, err)
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("release docs must not contain symlink %s", path)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect release doc %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("release docs contain unsupported file %s", path)
		}
		if err := copyReleaseFile(path, target); err != nil {
			return err
		}
		return nil
	})
}

func copyReleaseFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read release doc %s: %w", source, err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create release doc parent %s: %w", destination, err)
	}
	if err := os.WriteFile(destination, data, 0o644); err != nil {
		return fmt.Errorf("write release doc %s: %w", destination, err)
	}
	return nil
}
