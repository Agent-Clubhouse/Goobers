package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/goobers/goobers/internal/instance"
)

const releaseDocsVersionFile = "docs/RELEASE.md"

const (
	readmeSourceReleaseInstall = "## Install\n\n" +
		"Install the current `v0.1.0` release on Linux or macOS:\n\n" +
		"```sh\n" +
		"/bin/sh -c \"$(curl -fsSL https://github.com/Agent-Clubhouse/Goobers/releases/download/v0.1.0/install.sh)\" \\\n" +
		"  -- v0.1.0\n" +
		"```\n\n" +
		"The installer verifies the downloaded archive against the release checksum and\n" +
		"places `goobers` in `$HOME/.local/bin`. See\n" +
		"[Release installation and verification](docs/guides/releases.md) for\n" +
		"prerequisites, install-directory overrides, and the Windows path.\n\n"
	readmeSourceInstall = "## Learn Goobers or configure an instance\n\n" +
		"To **learn how Goobers works**, follow the\n" +
		"[quickstart tutorial](docs/guides/quickstart.md). It starts with an offline,\n" +
		"credential-free workflow and then uses a disposable repository for one\n" +
		"issue-to-PR run. The tutorial is intentionally separate from configuring a\n" +
		"real application.\n\n" +
		"To **configure a real instance**, choose one of these paths:\n\n" +
		"- Run `goobers init --guided` for the browser wizard. It teaches\n" +
		"  the core concepts while inspecting your repository, adapting the canonical\n" +
		"  workflows, preparing required repository metadata, and validating the\n" +
		"  resulting instance. It does not execute a workflow.\n" +
		"- Read [Onboard an arbitrary repository](docs/guides/arbitrary-repo-onboarding.md)\n" +
		"  and perform the same setup manually.\n" +
		"- Ask your coding agent to read that guide and the release-matched\n" +
		"  [Getting Started skill](skills/goobers-getting-started/SKILL.md), inspect your\n" +
		"  repository, explain each proposed write, and create the validated\n" +
		"  configuration for you.\n"
	quickstartSourceBuild = "## Build the binary\n\n```sh\n" +
		"go build -o bin/goobers ./cmd/goobers    # or: make build\n```\n\n"
	quickstartSourceInit = "## Separate path: configure a real instance\n\n" +
		"This section is not the next tutorial step. It summarizes the separate,\n" +
		"production-oriented path documented fully in\n" +
		"[Onboard an arbitrary repository](https://github.com/Agent-Clubhouse/Goobers/blob/main/docs/guides/arbitrary-repo-onboarding.md).\n\n" +
		"Start the focused browser walkthrough:\n\n" +
		"```sh\n" +
		"export PATH=\"$PWD/bin:$PATH\"\n" +
		"goobers init --guided\n" +
		"```\n\n" +
		"Provide an existing local Git clone. Getting Started supports GitHub and Azure\n" +
		"DevOps, discovers repository identity, default branch, CI command, toolchain,\n" +
		"and existing CLI authentication, then asks only for configuration placement,\n" +
		"workflow behavior, and agent harness choices that cannot be derived.\n\n" +
		"The workflow choices are adapted from the canonical modules under\n" +
		"[`config-examples/gaggles/acme-web`](../../config-examples/gaggles/acme-web/),\n" +
		"not from the deliberately simplified `quickstart@v1` tutorial workflow.\n\n"
	quickstartSourceManualInit = "### Manual/advanced alternative: bare `init`\n\n" +
		"Use bare init when you intentionally want to scaffold and edit every\n" +
		"configuration layer yourself:\n\n" +
		"```sh\n" +
		"goobers init ./my-instance\n" +
		"```\n\n" +
		"This creates `instance.yaml`, a starter `config/` (one gaggle, one goober, one\n" +
		"`default-implement` workflow), and the empty `gaggles/`, `scheduler/`, and\n" +
		"`telemetry.db` placeholders (ARCHITECTURE.md §6). The daemon creates each\n" +
		"gaggle's `runs/` and `workcopies/` beneath `gaggles/<gaggle>/`. Bare init is safe\n" +
		"to re-run because existing pieces are left untouched.\n\n" +
		"Before starting the instance, edit `my-instance/instance.yaml` to point at your\n" +
		"repository and set the referenced provider token (env var or file, never inline;\n" +
		"CFG-009/SEC-010). Edit `my-instance/config/` to shape the workforce: the gaggle's\n" +
		"`project` and `backlog` repo references, the goober's\n" +
		"`harness`/`skills`/`tools`, and the workflow's `triggers`/`tasks`/`gates`. Then\n" +
		"validate the manual configuration:\n\n" +
		"```sh\n" +
		"goobers validate ./my-instance\n" +
		"```\n\n" +
		"`validate` checks `instance.yaml` and every document under `config/` against the\n" +
		"canonical schemas. Exit codes are `0` for valid configuration, `1` for\n" +
		"validation errors, and `2` for usage or I/O errors.\n\n"
	quickstartInstalledInit = "## Separate path: configure a real instance\n\n" +
		"The release installer installs the binary and documentation only. Start browser-based setup after installation:\n\n" +
		"```sh\n" +
		"goobers init --guided\n" +
		"```\n\n" +
		"The legacy installer `--guided` option prints migration guidance and makes no changes; guided setup belongs to the installed `goobers` binary.\n\n"
	quickstartSourceOnboardingAssets = "Next, use the versioned `quickstart@v1` template for a first autonomous run\n" +
		"against a disposable GitHub repository you control. This path requires a\n" +
		"GitHub token and an authenticated agent harness. The shipped template's\n" +
		"goobers default to `harness: copilot`; to run it on Claude Code instead, pass\n" +
		"`--harness claude-code` to the `goobers init --template=quickstart` command\n" +
		"below, which seeds every goober with that harness and needs no `goober.yaml`\n" +
		"edit (see\n" +
		"[`config-examples/gaggles/acme-web-claude`](https://github.com/Agent-Clubhouse/Goobers/blob/main/config-examples/gaggles/acme-web-claude/)\n" +
		"for a full claude-code gaggle reference).\n\n" +
		"### Check prerequisites\n\n" +
		"The sample's CI command requires Node.js 20 or newer and npm. Confirm both are\n" +
		"available on the same `PATH` Goobers will use before materializing the sample:\n\n" +
		"```sh\n" +
		"node --version\n" +
		"npm --version\n" +
		"```\n\n" +
		"The first command must report `v20.0.0` or newer, and both commands must\n" +
		"succeed. At run start, Goobers preflights the configured `npm` CI executable\n" +
		"before any workflow stage executes. If npm is missing, the run fails before it\n" +
		"claims or changes an issue with a `ciCommand executable \"npm\" not found` error;\n" +
		"install Node.js 20+ and npm, then run the command again. The preflight checks\n" +
		"that npm exists, not the Node.js major version, so do not skip the literal\n" +
		"version checks above.\n\n" +
		"### Materialize the sample and the instance\n\n" +
		"Copy the paired sample into a separate throwaway directory, then scaffold the\n" +
		"instance that will operate on it:\n\n" +
		"```sh\n" +
		"bin/goobers onboarding stub-sample \\\n" +
		"  --destination ./getting-started-task-api \\\n" +
		"  --json\n" +
		"bin/goobers init --template=quickstart ./tutorial-instance\n" +
		"```\n\n" +
		"`stub-sample` is non-interactive, embeds the release-matched sample, and is\n" +
		"safe to re-run; it refuses conflicting user-owned files unless `--force` is\n" +
		"explicit, and never creates or pushes a remote itself. Its `--json` output is\n" +
		"a versioned action envelope:\n\n" +
		"```json\n" +
		"{\n" +
		"  \"action\": \"stub-sample\",\n" +
		"  \"version\": 2,\n" +
		"  \"created\": [\".github/workflows/ci.yml\", \"package.json\", \"...\"],\n" +
		"  \"skipped\": [],\n" +
		"  \"path\": \"/absolute/path/to/getting-started-task-api\",\n" +
		"  \"nextCommand\": \"goobers init --template=quickstart ./tutorial-instance\"\n" +
		"}\n" +
		"```\n\n" +
		"`created` lists paths written in this run; `skipped` lists paths already\n" +
		"present. `nextCommand` is the next command to run. `init --template=quickstart`\n" +
		"materializes `./tutorial-instance` still pointing at the template's\n" +
		"placeholder repository (`your-org/your-repo`); the next step replaces that\n" +
		"with a real one.\n\n" +
		"### Create a disposable repository and connect the instance to it\n\n" +
		"1. Create a new, empty GitHub repository to hold the sample, and push it —\n" +
		"   any name, delete it whenever you're done. With the GitHub CLI:\n\n" +
		"   ```sh\n" +
		"   gh repo create <owner>/<repo> --private --source ./getting-started-task-api --push\n" +
		"   ```\n\n" +
		"   Without it, create the repository at <https://github.com/new>, then:\n\n" +
		"   ```sh\n" +
		"   git -C ./getting-started-task-api init -b main\n" +
		"   git -C ./getting-started-task-api add -A\n" +
		"   git -C ./getting-started-task-api commit -m \"Getting Started sample\"\n" +
		"   git -C ./getting-started-task-api remote add origin https://github.com/<owner>/<repo>.git\n" +
		"   git -C ./getting-started-task-api push -u origin main\n" +
		"   ```\n\n" +
		"   Already have a disposable repository you'd rather reuse? Skip this step\n" +
		"   and use its `<owner>/<repo>` below instead.\n\n" +
		"2. Create a fine-grained GitHub PAT in\n" +
		"   [GitHub's token settings](https://github.com/settings/personal-access-tokens/new).\n" +
		"   **Set Resource owner to the account or organization that owns\n" +
		"   `<owner>/<repo>` (for example, `odsp-microsoft`); keep the default personal\n" +
		"   account when it owns the repository.** Choose **Only select repositories**\n" +
		"   and select exactly the disposable repository. Grant only **Contents: Read\n" +
		"   and write**, **Issues: Read and write**, and **Pull requests: Read and\n" +
		"   write**. Then export it once under the name `connect` expects by default:\n\n" +
		"   ```sh\n" +
		"   export GOOBERS_GITHUB_TOKEN=<your token>\n" +
		"   ```\n\n" +
		"3. Point the instance at the repository, and seed it in the same step:\n\n" +
		"   ```sh\n" +
		"   bin/goobers connect <owner>/<repo> --seed ./tutorial-instance\n" +
		"   ```\n\n" +
		"   `connect` rewrites the placeholder `your-org/your-repo` in\n" +
		"   `./tutorial-instance`'s `instance.yaml` and gaggle config to the repository\n" +
		"   you gave it, records `GOOBERS_GITHUB_TOKEN` (or the name you passed via\n" +
		"   `--token-env NAME`, if you keep the token under a different variable) as\n" +
		"   the credential reference by name only — the value never passes through\n" +
		"   this command — and validates the result in-process. `--seed` derives the\n" +
		"   labels the quickstart workflow's backlog selector requires, ensures they\n" +
		"   exist on the repository, and files one safe starter issue, using that same\n" +
		"   token — one `GOOBERS_GITHUB_TOKEN` export covers connecting and seeding.\n" +
		"   Configuration already pointing at a real repository is left alone unless\n" +
		"   you pass `--replace`.\n\n" +
		"4. Confirm Goobers can see and use your installed harness before the first\n" +
		"   run — `--check-harness` preflights every harness referenced by the\n" +
		"   instance's goobers and prints `HARNESS claude-code: OK` (or `HARNESS\n" +
		"   copilot: OK`) once the CLI is installed and signed in:\n\n" +
		"   ```sh\n" +
		"   bin/goobers validate --check-harness ./tutorial-instance\n" +
		"   ```\n\n" +
		"### Run it\n\n" +
		"```sh\n" +
		"bin/goobers run quickstart ./tutorial-instance\n" +
		"```\n\n" +
		"`run` waits for the run to reach a terminal state by default. This is a real\n" +
		"autonomous run against your disposable repository, so it takes noticeably\n" +
		"longer than the offline demo: it claims one approved issue, implements it,\n" +
		"performs an advisory code-review task, pushes the run branch, and opens a\n" +
		"pull request. It is **not for production**: it intentionally omits CI gates,\n" +
		"remediation loops, bounded escalation, merge policy, and issue close-out so\n" +
		"the onboarding happy path has no stall points.\n\n" +
		"`dashboard` blocks until interrupted, so open it in a second terminal to\n" +
		"browse the run in the Portal, and press Ctrl-C there when you're done:\n\n" +
		"```sh\n" +
		"# second terminal\n" +
		"bin/goobers dashboard ./tutorial-instance\n" +
		"```\n\n"
	quickstartSourceRun               = "```sh\ngoobers run <workflow>\n```"
	quickstartSourceStatusWorkflow    = "default-implement         example"
	quickstartInstalledStatusWorkflow = "implementation            example"
	linuxQuickstartSourceIntro        = "**Start here on Linux.** Complete sections 1 and 2 to install the required host\n" +
		"tools and build or install `goobers`. Then choose one route:\n\n" +
		"- return to the platform-neutral [quickstart tutorial](quickstart.md) for\n" +
		"  disposable learning; or\n" +
		"- continue with section 3 and\n" +
		"  [Onboard an arbitrary repository](arbitrary-repo-onboarding.md) to configure\n" +
		"  a real instance, using `goobers init --guided` if you want the\n" +
		"  browser wizard.\n\n" +
		"The remaining sections contain Linux-specific credential, live-run, and\n" +
		"systemd guidance. They supplement rather than duplicate either route.\n\n"
	linuxQuickstartSourceCIJob      = "CI job (`.github/workflows/ci.yml`), which runs the shipped binary end to end —\n"
	linuxQuickstartSourceToolchain  = "| Go toolchain | the version pinned in [`go.mod`](../../go.mod) (currently **1.26.6**) |\n"
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
		"# Go — install the toolchain matching go.mod (1.26.6). Distro packages often lag;\n" +
		"# prefer the official tarball:\n" +
		"curl -sSfL https://go.dev/dl/go1.26.6.linux-amd64.tar.gz | sudo tar -C /usr/local -xz\n" +
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
	_, err = stageOnboardingPayload(
		repoRoot,
		version,
		commit,
		filepath.Join(payloadDir, onboardingRoot),
	)
	if err != nil {
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
	releaseCommand := "goobers-" + version
	rewrites := []struct {
		path                 string
		sections             []onboardingSectionRewrite
		sourceCommandPrefix  string
		installedCommandName string
	}{
		{
			path: "README.md",
			sections: []onboardingSectionRewrite{
				{
					source:    readmeSourceReleaseInstall,
					installed: "",
				},
				{
					source: readmeSourceInstall,
					installed: fmt.Sprintf(
						"This copy is bundled with release `%s`. Use its versioned command so installing\n"+
							"a newer release cannot change this walkthrough:\n\n"+
							"```sh\n%s --version\n```\n\n"+
							"The fastest first run is the hermetic demo:\n\n"+
							"```sh\n"+
							"%s init --demo ./demo-instance\n"+
							"%s run demo ./demo-instance\n"+
							"%s trace <run-id> ./demo-instance\n"+
							"```\n\n"+
							"The demo runs the full curate -> implement -> review -> merge-preview loop on\n"+
							"Linux or macOS with mock providers, no credentials, and no network writes. From\n"+
							"there, graduate to\n"+
							"the token-bearing `quickstart@v1` template with\n"+
							"`%s init --template=quickstart ./tutorial-instance`, then a regular\n"+
							"instance created through guided init and the\n"+
							"production-oriented definitions under\n"+
							"[`config-examples/`](onboarding/templates/canonical/README.md).\n\n"+
							"The [full quickstart](docs/guides/quickstart.md) walks through that progression.\n\n"+
							"The release installer installs the binary and documentation only. Start setup with\n"+
							"`goobers init --guided` after installation.\n\n"+
							"If you opened this README directly from an extracted archive instead, replace `%s`\n"+
							"below with `./goobers`:\n\n"+
							"```sh\n"+
							"%s init --guided\n"+
							"%s run %s ./my-instance\n"+
							"```\n",
						version,
						releaseCommand,
						releaseCommand,
						releaseCommand,
						releaseCommand,
						releaseCommand,
						releaseCommand,
						releaseCommand,
						releaseCommand,
						instance.GuidedWorkflowImplementation,
					),
				},
			},
			sourceCommandPrefix:  "bin/goobers",
			installedCommandName: releaseCommand,
		},
		{
			path: "docs/guides/quickstart.md",
			sections: []onboardingSectionRewrite{
				{
					source: quickstartSourceOnboardingAssets,
					installed: strings.Replace(
						quickstartSourceOnboardingAssets,
						"bin/goobers",
						releaseCommand,
						1,
					),
				},
				{
					source: quickstartSourceBuild,
					installed: fmt.Sprintf(
						"## Confirm the installed binary\n\n"+
							"This copy is bundled with release `%s` and uses its versioned executable from `PATH`.\n\n"+
							"```sh\n%s --version\n```\n\n",
						version,
						releaseCommand,
					),
				},
				{
					source:    "../../config-examples/README.md",
					installed: "../../onboarding/templates/canonical/README.md",
				},
				{
					source:    "../../config-examples/gaggles/acme-web/workflows/implementation.yaml",
					installed: "../../onboarding/templates/canonical/gaggles/acme-web/workflows/implementation.yaml",
				},
				{
					source: quickstartSourceInit,
					installed: strings.Replace(
						quickstartInstalledInit,
						"goobers init --guided",
						releaseCommand+" init --guided",
						1,
					),
				},
				{
					source:    quickstartSourceManualInit,
					installed: "",
				},
				{
					source:    quickstartSourceRun,
					installed: "```sh\n" + releaseCommand + " run implementation ./my-instance\n```",
				},
				{
					source:    quickstartSourceStatusWorkflow,
					installed: quickstartInstalledStatusWorkflow,
				},
			},
			sourceCommandPrefix:  "bin/goobers",
			installedCommandName: releaseCommand,
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
							"it assumes the archive's `goobers` executable is installed on `PATH`. Return to\n"+
							"the tutorial for disposable learning, or run `goobers init --guided` to configure\n"+
							"a real instance through the browser wizard.\n\n",
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
