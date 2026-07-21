package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/proc"
)

// copilotAuthCheckArgs is the confirmed non-interactive Copilot authentication
// probe (#284/#271). The Copilot CLI has no auth-status subcommand, and
// `--version` succeeds even when signed out, so authentication is verified by a
// minimal, tool-disabled prompt: it exits 0 when the token is valid AND has the
// "Copilot Requests" fine-grained permission, and non-zero with an actionable
// auth error otherwise. `--available-tools=` (empty allowlist) disables every
// tool so the probe can never touch the filesystem or run shell commands;
// `--allow-all-tools` is still required to enable non-interactive mode.
//
// This runs in BOTH the operator-invoked `goobers validate --check-harness` and
// the automatic daemon-startup preflight (adapterFor wires it into every
// CopilotAdapter, so preflightAgenticHarnesses picks it up too — #238). It costs
// a real Copilot request (~a few AI credits, a couple of seconds), but
// preflightAgenticHarnesses runs once per process lifetime (once per `up` daemon
// boot, once per `run`), only for harnesses an agentic stage actually
// references — trivial next to the ~30-minute burned live-run a signed-out
// harness causes when the failure surfaces mid-run instead (the #284 incident).
var copilotAuthCheckArgs = []string{"-p", "Reply with exactly: ok", "--allow-all-tools", "--available-tools="}

// harnessPreflightTimeout bounds a single harness preflight (its version check
// plus the auth probe's real API round-trip) so a hung CLI or network can't
// hang `goobers validate` or `goobers up`/`run` startup.
const harnessPreflightTimeout = 90 * time.Second

const validateHelp = "Usage: goobers validate [--check-harness] [--check-repos] [--source-tree] [path]\n\n" +
	"Validate an instance's instance.yaml and config/ directory (default\n" +
	"path \".\"). --source-tree validates a checked-in config source tree\n" +
	"using instance.yaml.example and the path itself as config/. " +
	"--check-harness additionally preflights every agent harness\n" +
	"referenced by a goober (GBO-011) — installed, signed in, actionable\n" +
	"guidance otherwise. --check-repos resolves each target repository's\n" +
	"token and verifies authenticated git access. Exit codes: 0 = valid,\n" +
	"1 = validation errors, 2 = usage/IO error.\n"

func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	checkHarness := fs.Bool("check-harness", false, "also verify every referenced agent harness is installed and signed in")
	checkRepos := fs.Bool("check-repos", false, "also verify every target repository is reachable with its configured credential")
	sourceTree := fs.Bool("source-tree", false, "validate a checked-in config tree containing instance.yaml.example, manifest.yaml, and gaggles/")
	fs.Usage = helpUsage(stderr, "validate")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	l := instance.NewLayout(root)
	configFile := l.ConfigFile()
	configDir := l.ConfigDir()
	if *sourceTree {
		configFile = filepath.Join(root, "instance.yaml.example")
		configDir = root
	}
	if _, err := os.Stat(configFile); err != nil {
		if *sourceTree {
			pf(stderr, "error: %s not found (not a config source tree)\n", configFile)
		} else {
			pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", configFile)
		}
		return 2
	}

	cfg, err := instance.LoadConfig(configFile)
	if err != nil {
		pf(stdout, "INVALID instance.yaml:\n  %v\n", err)
		return 1
	}

	set, report, err := instance.LoadConfigDir(configDir)
	if err != nil && !errors.Is(err, instance.ErrInvalidConfig) {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if report != nil {
		for _, issue := range report.Issues {
			pln(stdout, issue.CLIString())
		}
	}
	if errors.Is(err, instance.ErrInvalidConfig) {
		pf(stdout, "\nconfig directory failed validation\n")
		return 1
	}

	// api/validate's cross-reference checks (above) mirror most of
	// workflow.Compile's own semantic analysis (CheckReachability/
	// CheckSchedules/CheckGateOutcomes/CheckAdmission), but this is the one
	// point that actually calls Compile with the same options `up`/`run` use
	// at daemon startup — including WithKnownChecks, which nothing else here
	// validates (#124). A config that fails this would also fail to start
	// the daemon; catching that now, at `validate` time, is the whole point.
	if _, err := compiledMachines(set, goobersByName(set)); err != nil {
		pf(stdout, "\nINVALID workflow: %v\n", err)
		return 1
	}

	// Docs-location existence (#1016). The config-load pass (api/validate) has
	// already rejected empty/absolute/escaping docs roots lexically; this adds
	// the filesystem half — a declared root that does not exist in the
	// repository — which api-level validation cannot do because it has no repo
	// tree. Resolved against the git working tree containing the config; skipped
	// (not failed) when the config is not inside a git repository, since there is
	// then no tree to check against.
	if !checkDocsRootsExist(root, set.Workflows, stdout) {
		return 1
	}

	if *checkHarness {
		if !checkHarnesses(set.Goobers, stdout, stderr) {
			return 1
		}
	}
	if *checkRepos && !checkTargetRepositories(cfg.Repos, stdout) {
		return 1
	}
	pf(stdout, "OK: instance.yaml valid; config/ valid (%d gaggle(s), %d goober(s), %d workflow(s))\n",
		len(set.Gaggles), len(set.Goobers), len(set.Workflows))
	return 0
}

// checkDocsRootsExist verifies every workflow-declared docs root exists in the
// repository (#1016). base is the user-supplied validate path (a config source
// tree or an instance root); the repository is its containing git working tree.
// It returns false (failing validation) when a declared root is missing, and
// true — skipping the check with a note — when base is not inside a git
// repository, since the lexical config-load checks already ran and there is no
// tree here to resolve roots against.
func checkDocsRootsExist(base string, workflows []apiv1.Workflow, stdout io.Writer) bool {
	type declaredRoot struct{ workflow, root string }
	var declared []declaredRoot
	for _, w := range workflows {
		for _, dr := range w.Spec.DocsRoots {
			declared = append(declared, declaredRoot{workflow: w.Name, root: dr})
		}
	}
	if len(declared) == 0 {
		return true
	}
	repoRoot, err := gitToplevel(base)
	if err != nil {
		pf(stdout, "DOCSROOTS: skipped existence check (%s is not inside a git repository)\n", base)
		return true
	}
	ok := true
	for _, d := range declared {
		clean := filepath.Clean(strings.TrimSpace(d.root))
		full := filepath.Join(repoRoot, clean)
		if _, statErr := os.Stat(full); statErr != nil {
			pf(stdout, "DOCSROOTS Workflow/%s: declared docs root %q does not exist in the repository (%s)\n",
				d.workflow, d.root, repoRoot)
			ok = false
		}
	}
	return ok
}

func gitToplevel(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

const (
	repositoryPreflightTimeout = 30 * time.Second
	repositoryKillWaitDelay    = time.Second
)

var targetRepositoryReachable = gitRepositoryReachable

func checkTargetRepositories(repos []instance.RepoRef, stdout io.Writer) bool {
	if len(repos) == 0 {
		pln(stdout, "REPOSITORY: no target repositories configured; nothing to check")
		return true
	}
	ok := true
	for i, repo := range repos {
		label := fmt.Sprintf("repos[%d] %s/%s", i, repo.Owner, repo.Name)
		refName := fmt.Sprintf("validate-repo-%d", i)
		var token string
		resolver, err := credentials.NewResolver([]credentials.TokenRef{{
			Name: refName,
			Env:  repo.Token.Env,
			File: repo.Token.File,
		}})
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), repositoryPreflightTimeout)
			token, err = resolver.Resolve(ctx, refName)
			if err == nil {
				err = targetRepositoryReachable(ctx, repo, token)
			}
			cancel()
		}
		if err != nil {
			pf(stdout, "REPOSITORY %s: unreachable: %s\n", label, scrubRepositoryError(err, token))
			pf(stdout, "  Check the owner/name, token source, repository access, and network connection.\n")
			ok = false
			continue
		}
		pf(stdout, "REPOSITORY %s: reachable\n", label)
	}
	return ok
}

func gitRepositoryReachable(ctx context.Context, repo instance.RepoRef, token string) error {
	if repo.Provider != "github" {
		return fmt.Errorf("provider %q does not support repository preflight", repo.Provider)
	}
	url := fmt.Sprintf("https://github.com/%s/%s.git", repo.Owner, repo.Name)
	cmd := exec.Command("git",
		"-c", "credential.helper=",
		"-c", "credential.interactive=never",
		"ls-remote", url,
	)
	cmd.Env = append(gitAuthEnv(token), "GIT_TERMINAL_PROMPT=0")

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	// Spawn in its own session (proc) so the ctx-timeout path below can kill
	// the whole git subprocess tree, not just the direct child.
	tree, err := proc.Start(cmd)
	if err != nil {
		return err
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case err = <-waitDone:
	case <-ctx.Done():
		_ = tree.Kill()
		select {
		case <-waitDone:
		case <-time.After(repositoryKillWaitDelay):
			// A descendant may have escaped the group while retaining an
			// output pipe. Do not let cmd.Wait block validation indefinitely.
		}
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	detail := strings.TrimSpace(output.String())
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

func scrubRepositoryError(err error, token string) string {
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte(token))
	scrubber := journal.Chain(registry, journal.NewPatternScrubber())
	return string(scrubber.Scrub([]byte(err.Error())))
}

// harnessAdapterFor is the harness-adapter lookup checkHarnesses uses.
// Package-level so tests can substitute a fake lookup without depending on a
// real, installed, signed-in Copilot CLI.
var harnessAdapterFor = adapterFor

// checkHarnesses preflights every distinct harness referenced by set's
// goobers (GBO-011), printing actionable guidance per failure. Returns false
// if any harness failed its preflight.
func checkHarnesses(goobers []apiv1.Goober, stdout, stderr io.Writer) bool {
	seen := map[apiv1.Harness]bool{}
	ok := true
	for _, g := range goobers {
		h := g.Spec.Harness
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true

		adapter, err := harnessAdapterFor(h)
		if err != nil {
			pf(stdout, "HARNESS %s: %v\n", h, err)
			ok = false
			continue
		}
		// The auth probe is wired into adapterFor itself (#238), so both this
		// check and the automatic daemon-startup preflight verify sign-in, not
		// just CLI presence — a fine-grained PAT lacking the "Copilot Requests"
		// permission (#284) passes --version but fails the probe.
		ctx, cancel := context.WithTimeout(context.Background(), harnessPreflightTimeout)
		err = adapter.Preflight(ctx)
		cancel()
		if err != nil {
			pf(stdout, "HARNESS %s: %v\n", h, err)
			ok = false
			continue
		}
		pf(stdout, "HARNESS %s: OK\n", h)
	}
	return ok
}

// adapterFor returns the registered adapter for a goober-declared harness kind.
//
// The CopilotAdapter carries copilotAuthCheckArgs so every preflight — the
// operator-invoked `validate --check-harness` AND the automatic daemon-startup
// preflight (preflightAgenticHarnesses) — verifies sign-in, not just CLI
// presence (#238). Both look the harness up through here, so wiring the probe
// once here is what closes #238's "catch a signed-out harness at startup, not
// mid-run" criterion.
func adapterFor(h apiv1.Harness) (harness.Adapter, error) {
	registry, err := buildHarnessRegistry(nil)
	if err != nil {
		return nil, err
	}
	return registry.Get(string(h))
}
