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
	quickstartSourceInit = "## 2. `init` — scaffold an instance root\n\n" +
		"```sh\n" +
		"bin/goobers init ./my-instance\n" +
		"```\n\n" +
		"Creates `instance.yaml`, a starter `config/` (one gaggle, one goober, one\n" +
		"implement-only workflow), and the empty `gaggles/`, `scheduler/`, and\n" +
		"`telemetry.db` placeholders (ARCHITECTURE.md §6). The daemon creates each\n" +
		"gaggle's `runs/` and `workcopies/` beneath `gaggles/<gaggle>/`. Safe to re-run — existing\n" +
		"pieces are left untouched.\n\n"
	quickstartInstalledInit = "## 2. Use the guided instance\n\n" +
		"The release installer and bundled README already run guided setup for\n" +
		"`./my-instance`; if you used either path, do not initialize it again. Continue\n" +
		"with step 3 below.\n\n" +
		"If you opened this guide directly from an extracted archive, create the same\n" +
		"guided instance now:\n\n" +
		"```sh\n" +
		"goobers init --guided ./my-instance\n" +
		"```\n\n" +
		"Keep the default workflow selection, or explicitly select `implementation`, so\n" +
		"the first manual run below uses the workflow the guided setup created. Guided\n" +
		"setup validates the instance and refuses an already configured target.\n\n"
	quickstartSourceRun               = "bin/goobers run default-implement ./my-instance"
	quickstartInstalledRun            = "goobers run implementation ./my-instance"
	quickstartSourceStatusWorkflow    = "default-implement         example"
	quickstartInstalledStatusWorkflow = "implementation            example"
	linuxQuickstartSourceIntro        = "Stand up the `goobers` daemon on a Linux host from scratch: install prerequisites,\n" +
		"build, configure credentials, and drive a first run. This is the Linux-specific\n" +
		"companion to the platform-neutral [`quickstart.md`](quickstart.md) — the CLI\n" +
		"surface is identical; this page calls out the few things that are Linux-specific\n" +
		"and records the exact environment Goobers is validated on.\n\n"
	linuxQuickstartSourceCIJob      = "CI job (`.github/workflows/ci.yml`), which runs the shipped binary end to end —\n"
	linuxQuickstartSourceToolchain  = "| Go toolchain | the version pinned in [`go.mod`](../../go.mod) (currently **1.26.5**) |\n"
	linuxQuickstartSourceValidation = "> **Linux delta — deterministic `network: none` stages use user namespaces.** On\n" +
		"> Linux, a workflow stage that declares `network: none` is isolated with an\n" +
		"> unprivileged user + network namespace (`CLONE_NEWUSER`), not an external\n" +
		"> sandbox. This works out of the box on the validated Ubuntu 24.04 runner. Some\n" +
		"> hardened distros disable unprivileged user namespaces (e.g. a non-default\n" +
		"> `kernel.apparmor_restrict_unprivileged_userns=1` or\n" +
		"> `kernel.unprivileged_userns_clone=0`); if a deterministic stage fails to fork\n" +
		"> there, enable unprivileged user namespaces for the daemon's user. To reproduce locally on any POSIX host:\n\n" +
		"```sh\n" +
		"go build -o bin/goobers ./cmd/goobers\n" +
		"go run ./test/linuxvalidate -bin bin/goobers -out ./linux-validation-evidence\n" +
		"cat ./linux-validation-evidence/summary.md\n" +
		"```\n\n"
	linuxQuickstartSourcePrerequisites = "## 1. Install prerequisites\n\n" +
		"```sh\n" +
		"# Go — install the toolchain matching go.mod (1.26.5). Distro packages often lag;\n" +
		"# prefer the official tarball:\n" +
		"curl -sSfL https://go.dev/dl/go1.26.5.linux-amd64.tar.gz | sudo tar -C /usr/local -xz\n" +
		"export PATH=\"/usr/local/go/bin:$(go env GOPATH)/bin:$PATH\"\n\n" +
		"# Git (>= 2.17 — any supported Ubuntu/Debian is newer):\n" +
		"sudo apt-get update && sudo apt-get install --yes git\n\n" +
		"# golangci-lint — REQUIRED on the daemon's PATH (see the note in step 5):\n" +
		"curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v2.12.2/install.sh \\\n" +
		"  | sh -s -- -b \"$(go env GOPATH)/bin\" v2.12.2\n" +
		"```\n\n" +
		"> Node.js 24 + npm are only needed to build/test the **portal** or run the full\n" +
		"> `go run ./test/ci` gate — not to run the daemon. See\n" +
		"> [CONTRIBUTING.md](../../CONTRIBUTING.md#platform-prerequisites) for the dev gate.\n\n"
	linuxQuickstartSourceBuild = "## 2. Build the binary\n\n```sh\n" +
		"go build -o bin/goobers ./cmd/goobers    # or: make build\n" +
		"sudo install -m 0755 bin/goobers /usr/local/bin/goobers   # optional: put it on PATH\n```\n\n"
	linuxQuickstartSourceDaemonPath = "> **Linux delta — the daemon's PATH is not your shell's.** A workflow's\n" +
		"> `local-ci` stage runs `make ci`/`golangci-lint` as a *subprocess of the\n" +
		"> daemon*, inheriting the daemon process's environment, not your interactive\n" +
		"> dotfiles. Ensure `golangci-lint` and the Go toolchain are on the PATH the\n" +
		"> daemon sees. Under a systemd unit this is the unit's `Environment=PATH=…`\n" +
		"> (see supervision, below); when launched from a shell it is that shell's PATH.\n\n"
	linuxQuickstartSourceSupervision = "For an unattended node, run the daemon under **systemd** instead of a foreground\n" +
		"shell. A ready-to-edit user-service template and full install/start/stop/status/\n" +
		"logs/upgrade instructions are in\n" +
		"[Daemon supervision](supervision.md#linux-systemd) — including the template at\n" +
		"[`packaging/systemd/goobers.service`](../../packaging/systemd/goobers.service).\n\n"
)

type onboardingSectionRewrite struct {
	source    string
	installed string
}

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
		sections             []onboardingSectionRewrite
		sourceCommandPrefix  string
		installedCommandName string
	}{
		{
			path: "README.md",
			sections: []onboardingSectionRewrite{{
				source: readmeSourceInstall,
				installed: fmt.Sprintf(
					"This copy is bundled with release `%s` and assumes `goobers` is installed on `PATH`.\n\n"+
						"```sh\ngoobers --version\n"+
						"goobers init --guided ./my-instance\n\n",
					version,
				),
			}},
			sourceCommandPrefix:  "$HOME/.local/bin/goobers",
			installedCommandName: "goobers",
		},
		{
			path: "docs/guides/quickstart.md",
			sections: []onboardingSectionRewrite{
				{
					source: quickstartSourceBuild,
					installed: fmt.Sprintf(
						"## 1. Confirm the installed binary\n\n"+
							"This copy is bundled with release `%s` and uses the `goobers` executable from `PATH`.\n\n"+
							"```sh\ngoobers --version\n```\n\n",
						version,
					),
				},
				{
					source:    quickstartSourceInit,
					installed: quickstartInstalledInit,
				},
				{
					source:    quickstartSourceRun,
					installed: quickstartInstalledRun,
				},
				{
					source:    quickstartSourceStatusWorkflow,
					installed: quickstartInstalledStatusWorkflow,
				},
			},
			sourceCommandPrefix:  "bin/goobers",
			installedCommandName: "goobers",
		},
		{
			path: "docs/guides/quickstart-linux.md",
			sections: []onboardingSectionRewrite{
				{
					source: linuxQuickstartSourceIntro,
					installed: fmt.Sprintf(
						"Use the `goobers` daemon bundled with release `%s` on a Linux host: install\n"+
							"runtime prerequisites, configure credentials, and drive a first run. This is the\n"+
							"Linux-specific companion to the platform-neutral [`quickstart.md`](quickstart.md);\n"+
							"it assumes the archive's `goobers` executable is installed on `PATH`.\n\n",
						version,
					),
				},
				{
					source:    linuxQuickstartSourceCIJob,
					installed: "release CI job, which runs the shipped binary end to end —\n",
				},
				{
					source:    linuxQuickstartSourceToolchain,
					installed: "| Release binary | linux/amd64 binary built and verified by the release pipeline |\n",
				},
				{
					source: linuxQuickstartSourceValidation,
					installed: "> **Linux delta — deterministic `network: none` stages use user namespaces.** On\n" +
						"> Linux, a workflow stage that declares `network: none` is isolated with an\n" +
						"> unprivileged user + network namespace (`CLONE_NEWUSER`), not an external\n" +
						"> sandbox. This works out of the box on the validated Ubuntu 24.04 runner. Some\n" +
						"> hardened distros disable unprivileged user namespaces; if a deterministic\n" +
						"> stage fails to fork there, enable unprivileged user namespaces for the daemon's user.\n\n" +
						"The source-only Linux validation harness is not included in release archives; its\n" +
						"evidence is produced by the release's CI run before packaging.\n\n",
				},
				{
					source: linuxQuickstartSourcePrerequisites,
					installed: "## 1. Install runtime prerequisites\n\n" +
						"The packaged `goobers` binary is self-contained; Go, Node.js, and build tools are\n" +
						"not required to run it. Install Git (version 2.17 or newer):\n\n" +
						"```sh\nsudo apt-get update && sudo apt-get install --yes git\n```\n\n" +
						"Workflow stages may require additional tools from the repositories they operate on.\n\n",
				},
				{
					source: linuxQuickstartSourceBuild,
					installed: fmt.Sprintf(
						"## 2. Confirm the installed binary\n\n"+
							"This copy is bundled with release `%s`; confirm the archive's executable from `PATH`:\n\n"+
							"```sh\ngoobers --version\n```\n\n",
						version,
					),
				},
				{
					source: linuxQuickstartSourceDaemonPath,
					installed: "> **Linux delta — the daemon's PATH is not your shell's.** Workflow stage\n" +
						"> commands run as subprocesses of the daemon and inherit its environment, not your\n" +
						"> interactive dotfiles. Ensure every tool used by your configured workflows is on\n" +
						"> the PATH the daemon sees. Under systemd, set that PATH in the unit's environment.\n\n",
				},
				{
					source: linuxQuickstartSourceSupervision,
					installed: "For an unattended node, run the daemon under **systemd** instead of a foreground\n" +
						"shell. The bundled [Daemon supervision](supervision.md#linux-systemd) guide includes\n" +
						"a ready-to-edit user-service template and full lifecycle instructions.\n\n",
				},
			},
		},
	}

	for _, rewrite := range rewrites {
		path := filepath.Join(payloadDir, filepath.FromSlash(rewrite.path))
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read release onboarding doc %s: %w", rewrite.path, err)
		}
		content := string(data)
		for _, section := range rewrite.sections {
			if strings.Count(content, section.source) != 1 {
				return fmt.Errorf("release onboarding source section drifted in %s", rewrite.path)
			}
			content = strings.Replace(content, section.source, section.installed, 1)
		}
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
