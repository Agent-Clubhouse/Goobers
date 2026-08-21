// Command ci runs the repository's fast, merge, and full validation tiers.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const versionPackage = "github.com/goobers/goobers/internal/version"

type toolchain struct {
	goCommand       string
	gofmtCommand    string
	gitCommand      string
	npmCommand      string
	golangciCommand string
}

type buildMetadata struct {
	version string
	commit  string
	date    string
}

type check struct {
	label        string
	command      string
	args         []string
	env          []string
	capture      bool
	expectEmpty  bool
	windowsBatch bool
	// group names the parallel CI job that owns this check. The full merge
	// tier (`go run ./test/ci`) runs every check regardless; `go run ./test/ci
	// group <name>` runs only the checks in one group so the workflow can fan
	// the sequential merge gate across independent runners. See ci.yml.
	group string
	// skip, when non-nil, is evaluated immediately before the check would
	// run. If it reports skip=true, the check is treated as an automatic
	// pass: the returned reason is logged in place of the command output
	// and the command itself is never invoked. nil means always run, same
	// as before this field existed. See resolvePortalPlaywrightSkip (#3372).
	skip func() (bool, string)
}

// Check groups. Each maps to one parallel job in .github/workflows/ci.yml.
// Membership is disjoint and exhaustive: every merge-tier check belongs to
// exactly one group (asserted by TestEveryMergeCheckHasAGroup).
const (
	// groupChecks is the fast fan-in: formatting, module hygiene, the
	// no-phone-home guard, vet, command builds, config validation, and the portal
	// build/test/contract chain — everything except the three heavyweight steps
	// below.
	groupChecks = "checks"
	// groupLint is golangci-lint (staticcheck/govet/revive/...) on its own runner.
	groupLint = "lint"
	// groupUnit is the race/coverage unit suite — the long pole.
	groupUnit = "unit"
	// groupShipped is the shipped-workflow contract suite.
	groupShipped = "shipped"
)

// knownGroups is the set of valid `group NAME` selectors, used to reject a
// mistyped group before any work runs (independent of working directory).
var knownGroups = map[string]bool{
	groupChecks:  true,
	groupLint:    true,
	groupUnit:    true,
	groupShipped: true,
}

type executor interface {
	run(check) ([]byte, error)
}

type processExecutor struct {
	stdout io.Writer
	stderr io.Writer
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fast := false
	group := ""
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "fast":
		fast = true
	case len(args) == 2 && args[0] == "group" && strings.TrimSpace(args[1]) != "":
		group = args[1]
		if !knownGroups[group] {
			_, _ = fmt.Fprintf(stderr, "ci: unknown check group %q\n", group)
			return 2
		}
	case len(args) == 2 && args[0] == "full" && strings.TrimSpace(args[1]) != "":
		exec := processExecutor{stdout: stdout, stderr: stderr}
		if err := executeChecks(exec, fullChecks(args[1]), stdout, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "ci: %v\n", err)
			return 1
		}
		return 0
	default:
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/ci [fast | group NAME | full MAKE_COMMAND]")
		return 2
	}

	commands, err := commandPackages("cmd")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ci: discover command packages: %v\n", err)
		return 1
	}

	tools := configuredToolchain(os.Getenv)
	exec := processExecutor{stdout: stdout, stderr: stderr}
	metadata := resolveBuildMetadata(exec, tools, time.Now, os.Getenv)
	validationChecks := checks(commands, tools, metadata, runtime.GOOS, os.Getenv("GOOBERS_TEST_TIMING_FILE"))
	if fast {
		validationChecks = fastChecks(validationChecks)
	}
	if group != "" {
		selected := groupChecksOnly(validationChecks, group)
		if len(selected) == 0 {
			_, _ = fmt.Fprintf(stderr, "ci: unknown check group %q\n", group)
			return 2
		}
		validationChecks = applyRuntimeToggles(selected, os.Getenv)
	}
	if err := executeChecks(exec, validationChecks, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "ci: %v\n", err)
		return 1
	}
	return 0
}

// groupChecksOnly keeps only the checks belonging to one parallel CI job,
// preserving their merge-gate order (dependencies such as portal-install
// before portal-test are honoured because the source order is preserved).
func groupChecksOnly(all []check, group string) []check {
	var result []check
	for _, current := range all {
		if current.group == group {
			result = append(result, current)
		}
	}
	return result
}

// applyRuntimeToggles adapts a group's checks to the runner it executes on:
//
//   - GOOBERS_CI_RACE=0 strips -race from the unit and shipped-workflow suites.
//     Data races are a source-level property, so a single -race platform
//     (Linux) suffices; a second OS can run the same behavioural suite without
//     the ~3-5x instrumentation cost while preserving OS-specific coverage.
//   - GOOBERS_CI_SHARD=i/n splits the unit suite across n runners (1-based i),
//     dropping the coverage profile (partial per shard; the coverage *gate* is
//     the separate full-tier cover-check). Timing capture stays with the
//     unsharded owner (it only emits when GOOBERS_TEST_TIMING_FILE is set).
//   - GOOBERS_CI_TEST_TIMEOUT raises the per-package timeout for slower
//     platforms without weakening the suite or changing its package set.
func applyRuntimeToggles(checks []check, getenv func(string) string) []check {
	raceEnabled := getenv("GOOBERS_CI_RACE") != "0"
	shard := strings.TrimSpace(getenv("GOOBERS_CI_SHARD"))
	testTimeout := strings.TrimSpace(getenv("GOOBERS_CI_TEST_TIMEOUT"))
	// GOOBERS_LINT_GOOS cross-lints for another platform (e.g. darwin) from a
	// Linux runner. It sets GOOS for the golangci-lint *subprocess* only — never
	// as an ambient GOOS, which would make `go run ./test/ci` build this tool
	// for the target OS and then fail to exec it on the host.
	lintGOOS := strings.TrimSpace(getenv("GOOBERS_LINT_GOOS"))
	result := make([]check, 0, len(checks))
	for _, current := range checks {
		if !raceEnabled {
			current.args = withoutArg(current.args, "-race")
		}
		if shard != "" && current.label == "test" {
			current.args = shardUnitArgs(current.args, shard)
		}
		if testTimeout != "" && (current.label == "test" || current.label == "shipped-workflows") {
			current.args = replaceFlagValue(current.args, "-timeout", testTimeout)
		}
		if lintGOOS != "" && current.label == "lint" {
			current.env = append(append([]string(nil), current.env...), "GOOS="+lintGOOS)
		}
		result = append(result, current)
	}
	return result
}

func replaceFlagValue(args []string, flag, value string) []string {
	result := append([]string(nil), args...)
	for i := 0; i+1 < len(result); i++ {
		if result[i] == flag {
			result[i+1] = value
			break
		}
	}
	return result
}

func withoutArg(args []string, drop string) []string {
	result := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == drop {
			continue
		}
		result = append(result, arg)
	}
	return result
}

// shardUnitArgs injects `--shard i/n` into the hermetic runner invocation
// (before the `--` that separates hermetic flags from go-test arguments) and
// drops the coverage profile, which is only meaningful over the whole tree.
func shardUnitArgs(args []string, shard string) []string {
	result := make([]string, 0, len(args)+2)
	for _, arg := range args {
		switch arg {
		case "--":
			result = append(result, "--shard", shard, "--")
		case "-covermode=atomic", "-coverprofile=coverage.out":
			// Partial coverage per shard is meaningless; skip it.
		default:
			result = append(result, arg)
		}
	}
	return result
}

func configuredToolchain(getenv func(string) string) toolchain {
	return toolchain{
		goCommand:       envOrDefault(getenv, "GO", "go"),
		gofmtCommand:    envOrDefault(getenv, "GOFMT", "gofmt"),
		gitCommand:      envOrDefault(getenv, "GIT", "git"),
		npmCommand:      envOrDefault(getenv, "NPM", "npm"),
		golangciCommand: envOrDefault(getenv, "GOLANGCI_LINT", "golangci-lint"),
	}
}

func resolveBuildMetadata(exec executor, tools toolchain, now func() time.Time, getenv func(string) string) buildMetadata {
	return buildMetadata{
		version: envOrCommand(getenv, "VERSION", exec, tools.gitCommand, []string{"describe", "--tags", "--always", "--dirty"}, "dev"),
		commit:  envOrCommand(getenv, "COMMIT", exec, tools.gitCommand, []string{"rev-parse", "--short", "HEAD"}, "none"),
		date:    envOrDefault(getenv, "DATE", now().UTC().Format("2006-01-02T15:04:05Z")),
	}
}

func envOrCommand(
	getenv func(string) string,
	name string,
	exec executor,
	command string,
	args []string,
	fallback string,
) string {
	if value := getenv(name); value != "" {
		return value
	}
	output, err := exec.run(check{command: command, args: args, capture: true})
	if err != nil {
		return fallback
	}
	if value := strings.TrimSpace(string(output)); value != "" {
		return value
	}
	return fallback
}

func envOrDefault(getenv func(string) string, name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}
	return fallback
}

func commandPackages(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	var commands []string
	for _, entry := range entries {
		if entry.IsDir() {
			commands = append(commands, entry.Name())
		}
	}
	sort.Strings(commands)
	return commands, nil
}

func checks(commands []string, tools toolchain, metadata buildMetadata, goos, timingOutput string) []check {
	ldflags := fmt.Sprintf(
		"-X %s.Version=%s -X %s.Commit=%s -X %s.Date=%s",
		versionPackage, metadata.version,
		versionPackage, metadata.commit,
		versionPackage, metadata.date,
	)

	result := []check{
		{
			label:       "fmt-check",
			command:     tools.gofmtCommand,
			args:        []string{"-l", "."},
			capture:     true,
			expectEmpty: true,
			group:       groupChecks,
		},
		{label: "tidy-check", command: tools.goCommand, args: []string{"mod", "tidy", "-diff"}, group: groupChecks},
		{label: "no-phone-home", command: tools.goCommand, args: []string{"run", "./test/nophonehome"}, group: groupChecks},
		{label: "stage-name-lint", command: tools.goCommand, args: []string{"run", "./test/stagenamelint"}, group: groupChecks},
		{label: "vet", command: tools.goCommand, args: []string{"vet", "./..."}, group: groupChecks},
		{label: "flake-policy", command: tools.goCommand, args: []string{"run", "./test/flakepolicy"}, group: groupChecks},
		{label: "design-doc-status", command: tools.goCommand, args: []string{"run", "./test/designstatus"}, group: groupChecks},
		{label: "markdown-links", command: tools.goCommand, args: []string{"run", "./test/markdownlinks"}, group: groupChecks},
		// The release image's Go toolchain is an input to a shipped artifact,
		// and packaging/docker/Dockerfile is the only leg that can drift from
		// go.mod (ci.yml defers to it via go-version-file). #3452.
		{label: "go-toolchain", command: tools.goCommand, args: []string{"run", "./test/gotoolchain"}, group: groupChecks},
	}

	portalPrepared := false
	for _, command := range commands {
		if command == "goobers" {
			result = append(result, portalPreparationChecks(tools)...)
			portalPrepared = true
		}

		output := filepath.Join("bin", command)
		if goos == "windows" {
			output += ".exe"
		}
		result = append(result, check{
			label:   "build-" + command,
			command: tools.goCommand,
			args: []string{
				"build",
				"-ldflags", ldflags,
				"-o", output,
				"./cmd/" + command,
			},
			group: groupChecks,
		})
		if command == "goobers" {
			result = append(result, check{
				label:   "validate-configs",
				command: tools.goCommand,
				args:    []string{"run", "./test/configvalidate", output},
				group:   groupChecks,
			})
		}
	}
	if !portalPrepared {
		result = append(result, portalPreparationChecks(tools)...)
	}

	testEnvironment := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.fsync",
		"GIT_CONFIG_VALUE_0=none",
		"GOOBERS_DISABLE_FSYNC=1",
		"GOENV=off",
		"GOFLAGS=-mod=readonly",
		"GONOPROXY=none",
		"GONOSUMDB=none",
		"GOPRIVATE=",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
		"GOVCS=*:off",
	}
	if goos == "windows" {
		// The Windows race detector uses cgo and a compatible MinGW-w64 compiler.
		testEnvironment = append(testEnvironment, "CGO_ENABLED=1")
	}

	testArgs := []string{
		"run", "./test/hermetic",
		"--go-command", tools.goCommand,
	}
	if timingOutput != "" {
		testArgs = append(testArgs, "--timing-job", "unit", "--timing-output", timingOutput)
	}
	testArgs = append(testArgs,
		"--",
		"-race",
		"-timeout", "30m",
		"-covermode=atomic",
		"-coverprofile=coverage.out",
		"./...",
	)
	testCheck := check{
		label:   "test",
		command: tools.goCommand,
		// -timeout 30m raises the per-package ceiling above Go's 10m default
		// purely as headroom against macOS hosted-runner contention (#1124):
		// the cmd/goobers integration package legitimately runs long under a
		// loaded runner, and a timeout there panics the whole suite. The hermetic
		// environment also excludes an ambient OTLP collector so unavailable
		// infrastructure cannot consume this headroom with exporter retries.
		// Normal runs finish much sooner, so the higher ceiling never slows a
		// green run.
		args: testArgs,
		env: append(
			append([]string(nil), testEnvironment...),
			"GOOBERS_SKIP_SHIPPED_WORKFLOW_CONTRACTS=1",
		),
		group: groupUnit,
	}
	schemaDescriptionCoverageCheck := check{
		label:   "schema-description-coverage",
		command: tools.goCommand,
		args:    []string{"test", "-v", "-run", "^TestDescriptionCoverage$", "./api/schemas"},
		env:     testEnvironment,
		group:   groupUnit,
	}
	shippedWorkflowCheck := check{
		label:   "shipped-workflows",
		command: tools.goCommand,
		args:    []string{"test", "-race", "-timeout", "20m", "-count=1", "./test/shippedworkflows"},
		env:     testEnvironment,
		group:   groupShipped,
	}

	result = append(result,
		shippedWorkflowCheck,
		schemaDescriptionCoverageCheck,
		testCheck,
		check{
			label:   "lint",
			command: tools.golangciCommand,
			args:    []string{"run", "--allow-serial-runners"},
			env:     golangciCacheEnvironment(),
			group:   groupLint,
		},
		check{
			label:        "portal-test",
			command:      tools.npmCommand,
			args:         []string{"--prefix", "portal", "test"},
			windowsBatch: true,
			group:        groupChecks,
		},
		check{
			label:        "portal-deadcode",
			command:      tools.npmCommand,
			args:         []string{"--prefix", "portal", "run", "deadcode"},
			windowsBatch: true,
			group:        groupChecks,
		},
		check{
			label:        "portal-e2e",
			command:      tools.npmCommand,
			args:         []string{"--prefix", "portal", "run", "test:e2e"},
			windowsBatch: true,
			group:        groupChecks,
		},
		check{
			label:   "portal-contract-generate",
			command: tools.goCommand,
			args:    []string{"generate", "./internal/apicontract"},
			group:   groupChecks,
		},
		check{
			label:   "portal-contract-diff",
			command: tools.gitCommand,
			args: []string{
				"diff", "--exit-code", "--",
				"portal/src/api/contract.generated.ts",
				"portal/src/api/wire.generated.ts",
			},
			group: groupChecks,
		},
		check{
			label:        "portal-contract-typecheck",
			command:      tools.npmCommand,
			args:         []string{"--prefix", "portal", "run", "typecheck"},
			windowsBatch: true,
			group:        groupChecks,
		},
		check{
			label:        "portal-contract-test",
			command:      tools.npmCommand,
			args:         []string{"--prefix", "portal", "run", "test:contract"},
			windowsBatch: true,
			group:        groupChecks,
		},
		// The CRD manifests are a published contract — a field present in the
		// Go API types but absent from config/crd/bases is a contract that
		// silently lies about the DSL, and the manifests have drifted before
		// with nothing going red. Mirrors portal-contract-generate/-diff: a
		// pinned-tool regen (controller-gen@v0.16.5, matching Makefile's
		// CONTROLLER_GEN_VERSION — keep both in sync) followed by a git diff
		// that fails the gate on any drift.
		check{
			label:   "manifests-generate",
			command: tools.goCommand,
			args: []string{
				"run", "sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5",
				"crd:allowDangerousTypes=true", "paths=./api/v1alpha1/...", "output:crd:dir=config/crd/bases",
			},
			group: groupChecks,
		},
		check{
			label:   "manifests-diff",
			command: tools.gitCommand,
			args:    []string{"diff", "--exit-code", "--", "config/crd/bases"},
			group:   groupChecks,
		},
	)
	return result
}

func fullChecks(makeCommand string) []check {
	targets := []string{
		"ci",
		"test-integration-strict",
		"test-e2e",
		"test-conformance",
		"test-envtest",
		"cover-check",
		"sandbox-check",
		"linux-node-validation",
		"stress",
	}
	result := make([]check, 0, len(targets))
	for _, target := range targets {
		result = append(result, check{
			label:   target,
			command: makeCommand,
			args:    []string{target},
		})
	}
	return result
}

func fastChecks(mergeChecks []check) []check {
	result := make([]check, 0, len(mergeChecks))
	for _, current := range mergeChecks {
		if current.label == "fmt-check" ||
			current.label == "no-phone-home" ||
			current.label == "vet" ||
			strings.HasPrefix(current.label, "build-") {
			result = append(result, current)
		}
	}
	return result
}

// playwrightBrowsersPathEnv is the environment variable Playwright's CLI and
// installer both honour for the browsers cache directory.
const playwrightBrowsersPathEnv = "PLAYWRIGHT_BROWSERS_PATH"

// resolvePortalPlaywrightSkip decides whether portal-playwright-install can
// no-op instead of invoking `playwright install chromium`.
//
// Playwright's own installer (packages/playwright-core/src/server/registry/
// index.ts, Registry.install) unconditionally mkdir's PLAYWRIGHT_BROWSERS_PATH
// and takes a lock on a __dirlock file inside it *before* it ever compares
// installed browser versions. On a container image that bakes browsers into
// a read-only PLAYWRIGHT_BROWSERS_PATH (readOnlyRootFilesystem), that lock
// write fails outright — `EROFS: read-only file system, mkdir
// '.../__dirlock'` — even though the exact pinned chromium build the repo
// needs is already sitting right there. See #3372.
//
// This mirrors just enough of Playwright's own registry logic to detect that
// already-satisfied case without ever invoking the installer: a browser's
// on-disk directory is named "<name>-<revision>" and a completed download
// leaves an "INSTALLATION_COMPLETE" marker file inside it (both from the
// same index.ts). The revision is read from the installed playwright-core
// package's own browsers.json manifest rather than re-deriving it from
// portal/package-lock.json: npm ci (the portal-install check immediately
// before this one, in the same group) already resolved package-lock.json's
// pinned @playwright/test version into that exact file, so it names the one
// chromium revision that version can ever mean — with no separate
// version-to-revision table of our own to keep in sync with playwright
// releases.
//
// If PLAYWRIGHT_BROWSERS_PATH is unset, or the pinned revision can't be
// resolved, or that revision isn't present with a completion marker, this
// reports "do not skip" and portal-playwright-install runs the installer
// exactly as it does today — a missing or wrong-version bake must still
// fail loudly here, not surface later as a confusing portal-test failure.
func resolvePortalPlaywrightSkip() (bool, string) {
	browsersDir := strings.TrimSpace(os.Getenv(playwrightBrowsersPathEnv))
	if browsersDir == "" {
		return false, fmt.Sprintf("%s not set", playwrightBrowsersPathEnv)
	}
	manifestPath := filepath.Join("portal", "node_modules", "playwright-core", "browsers.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return false, fmt.Sprintf("could not read %s: %v", manifestPath, err)
	}
	revision, err := pinnedChromiumRevision(data)
	if err != nil {
		return false, fmt.Sprintf("could not resolve pinned chromium revision from %s: %v", manifestPath, err)
	}
	chromiumDir := filepath.Join(browsersDir, "chromium-"+revision)
	if _, err := os.Stat(filepath.Join(chromiumDir, "INSTALLATION_COMPLETE")); err != nil {
		return false, fmt.Sprintf("chromium revision %s not installed at %s", revision, chromiumDir)
	}
	return true, fmt.Sprintf("browsers preinstalled at %s, skipping", chromiumDir)
}

// pinnedChromiumRevision extracts the chromium build revision from a
// playwright-core browsers.json manifest — the exact file Playwright's own
// installer reads to decide what to download.
func pinnedChromiumRevision(browsersJSON []byte) (string, error) {
	var manifest struct {
		Browsers []struct {
			Name     string `json:"name"`
			Revision string `json:"revision"`
		} `json:"browsers"`
	}
	if err := json.Unmarshal(browsersJSON, &manifest); err != nil {
		return "", fmt.Errorf("parse browsers.json: %w", err)
	}
	for _, browser := range manifest.Browsers {
		if browser.Name == "chromium" {
			if strings.TrimSpace(browser.Revision) == "" {
				return "", fmt.Errorf("chromium entry has no revision")
			}
			return browser.Revision, nil
		}
	}
	return "", fmt.Errorf("no chromium entry")
}

func portalPreparationChecks(tools toolchain) []check {
	return []check{
		{
			label:        "portal-install",
			command:      tools.npmCommand,
			args:         []string{"--prefix", "portal", "ci", "--no-audit", "--no-fund"},
			windowsBatch: true,
			group:        groupChecks,
		},
		{
			label:        "portal-playwright-install",
			command:      tools.npmCommand,
			args:         []string{"--prefix", "portal", "exec", "--", "playwright", "install", "chromium"},
			windowsBatch: true,
			skip:         resolvePortalPlaywrightSkip,
			group:        groupChecks,
		},
		{
			label:        "portal-build",
			command:      tools.npmCommand,
			args:         []string{"--prefix", "portal", "run", "build"},
			windowsBatch: true,
			group:        groupChecks,
		},
		// portal-build writes the production bundle into cmd/goobers/portal-dist,
		// the //go:embed-ed and committed directory the daemon serves. Nothing
		// diffed the rebuild against what's committed, so a stale bundle could
		// ship silently (a source change merged without re-running the build).
		// This guards it exactly like portal-contract-diff guards the generated
		// wire types: a fresh build that differs from the committed bundle fails
		// the gate. It runs immediately after the build (before the multi-minute
		// test step) so drift fails fast. vite cleans the outDir on build, so a
		// content change deletes the old content-hashed asset — a tracked
		// deletion git diff --exit-code reports — in addition to rewriting
		// index.html, so real drift never slips through. #1110.
		{
			label:   "portal-dist-diff",
			command: tools.gitCommand,
			args:    []string{"diff", "--exit-code", "--", "cmd/goobers/portal-dist"},
			group:   groupChecks,
		},
		// portal-dist-diff above only reports TRACKED-file changes — a newly
		// added file the build produced (e.g. the first plain, non-hashed
		// portal/public/ asset vite copies verbatim, referenced by no hashed
		// chunk) sits untracked and passes that diff clean, even though it's
		// missing from every other checkout's git history until someone
		// remembers to `git add` it. `git status --porcelain` reports
		// untracked files too, closing that blind spot. #2056.
		{
			label:       "portal-dist-untracked",
			command:     tools.gitCommand,
			args:        []string{"status", "--porcelain", "--", "cmd/goobers/portal-dist"},
			capture:     true,
			expectEmpty: true,
			group:       groupChecks,
		},
	}
}

func executeChecks(exec executor, checks []check, stdout, stderr io.Writer) error {
	return executeChecksAt(exec, checks, stdout, stderr, time.Now)
}

func executeChecksAt(
	exec executor,
	checks []check,
	stdout, stderr io.Writer,
	now func() time.Time,
) error {
	for _, current := range checks {
		_, _ = fmt.Fprintf(stdout, "==> %s\n", current.label)
		started := now()
		if current.skip != nil {
			if shouldSkip, reason := current.skip(); shouldSkip {
				_, _ = fmt.Fprintf(stdout, "%s: %s\n", current.label, reason)
				_, _ = fmt.Fprintf(stdout, "<== %s (elapsed %s)\n", current.label, now().Sub(started).Round(time.Millisecond))
				continue
			}
		}
		output, err := exec.run(current)
		_, _ = fmt.Fprintf(stdout, "<== %s (elapsed %s)\n", current.label, now().Sub(started).Round(time.Millisecond))
		if err != nil {
			if len(output) > 0 {
				_, _ = stderr.Write(output)
				if output[len(output)-1] != '\n' {
					_, _ = fmt.Fprintln(stderr)
				}
			}
			return fmt.Errorf("%s: %w", current.label, err)
		}
		if current.expectEmpty && strings.TrimSpace(string(output)) != "" {
			// expectEmpty is a generic "this command's output must be empty
			// or the gate fails" contract (fmt-check's gofmt -l, #2056's
			// portal-dist-untracked git status --porcelain, ...) — the
			// message names the check, not any one consumer's meaning.
			_, _ = fmt.Fprintf(stdout, "%s produced unexpected output:\n", current.label)
			_, _ = stdout.Write(output)
			if output[len(output)-1] != '\n' {
				_, _ = fmt.Fprintln(stdout)
			}
			return fmt.Errorf("%s: expected no output", current.label)
		}
	}
	return nil
}

func (e processExecutor) run(current check) ([]byte, error) {
	command, args := commandInvocation(current, runtime.GOOS, os.Getenv)
	cmd := exec.Command(command, args...)
	if len(current.env) > 0 {
		cmd.Env = mergeEnvironment(os.Environ(), current.env, runtime.GOOS == "windows")
	}
	if current.capture {
		if current.expectEmpty {
			cmd.Stderr = e.stderr
		} else {
			cmd.Stderr = io.Discard
		}
		return cmd.Output()
	}
	cmd.Stdout = e.stdout
	cmd.Stderr = e.stderr
	return nil, cmd.Run()
}

func commandInvocation(current check, goos string, getenv func(string) string) (string, []string) {
	if goos != "windows" || !current.windowsBatch {
		return current.command, current.args
	}
	args := make([]string, 0, len(current.args)+4)
	args = append(args, "/d", "/s", "/c", current.command)
	args = append(args, current.args...)
	return envOrDefault(getenv, "ComSpec", "cmd.exe"), args
}

// golangciCacheEnvironment pins golangci-lint to a cache directory derived from
// the working directory, because its cache is neither concurrency-safe nor
// path-safe and Goobers runs N worktrees against one host at a time.
//
// A cached analysis result carries the path it was computed against, so a run in
// worktree A can be handed a diagnostic naming a file in sibling worktree B — a
// violation the failing run cannot fix, because the file is not in its checkout.
// The separate process-wide runner lock is handled by --allow-serial-runners.
//
// Keying on the working directory rather than minting a fresh directory keeps
// the cache warm for the common case of repeated runs in one checkout, while
// giving every worktree its own lock and its own path space. Falls back to
// inheriting the ambient environment when the working directory cannot be
// resolved: a shared cache is the status quo, not a regression.
func golangciCacheEnvironment() []string {
	workdir, err := os.Getwd()
	if err != nil {
		return nil
	}
	digest := sha256.Sum256([]byte(workdir))
	cache := filepath.Join(os.TempDir(), "goobers-golangci-lint", hex.EncodeToString(digest[:])[:16])
	return []string{"GOLANGCI_LINT_CACHE=" + cache}
}

func mergeEnvironment(base, overrides []string, caseInsensitive bool) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, variable := range base {
		name := environmentName(variable)
		overridden := false
		for _, override := range overrides {
			overrideName := environmentName(override)
			if name == overrideName || caseInsensitive && strings.EqualFold(name, overrideName) {
				overridden = true
				break
			}
		}
		if !overridden {
			result = append(result, variable)
		}
	}
	return append(result, overrides...)
}

func environmentName(variable string) string {
	if index := strings.IndexByte(variable, '='); index >= 0 {
		return variable[:index]
	}
	return variable
}
