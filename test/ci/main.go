// Command ci runs the repository's fast, merge, and full validation tiers.
package main

import (
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
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "fast":
		fast = true
	case len(args) == 2 && args[0] == "full" && strings.TrimSpace(args[1]) != "":
		exec := processExecutor{stdout: stdout, stderr: stderr}
		if err := executeChecks(exec, fullChecks(args[1]), stdout, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "ci: %v\n", err)
			return 1
		}
		return 0
	default:
		_, _ = fmt.Fprintln(stderr, "usage: go run ./test/ci [fast | full MAKE_COMMAND]")
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
	if err := executeChecks(exec, validationChecks, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "ci: %v\n", err)
		return 1
	}
	return 0
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
		},
		{label: "vet", command: tools.goCommand, args: []string{"vet", "./..."}},
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
		})
		if command == "goobers" {
			result = append(result, check{
				label:   "validate-configs",
				command: tools.goCommand,
				args:    []string{"run", "./test/configvalidate", output},
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
		"-timeout", "20m",
		"-covermode=atomic",
		"-coverprofile=coverage.out",
		"./...",
	)
	testCheck := check{
		label:   "test",
		command: tools.goCommand,
		// -timeout 20m raises the per-package ceiling above Go's 10m default
		// purely as headroom against macOS hosted-runner contention (#1124):
		// the cmd/goobers integration package legitimately runs long under a
		// loaded runner, and a timeout there panics the whole suite. This is
		// not masking a hang — the affected tests pass locally at high
		// -count and the OTLP-flush blocking that compounded it is fixed in
		// this change (telemetry soft-fails an unreachable collector). Normal
		// runs finish in ~2m, so the higher ceiling never slows a green run.
		args: testArgs,
		env:  testEnvironment,
	}

	result = append(result,
		testCheck,
		check{label: "lint", command: tools.golangciCommand, args: []string{"run"}},
		check{
			label:        "portal-test",
			command:      tools.npmCommand,
			args:         []string{"--prefix", "portal", "test"},
			windowsBatch: true,
		},
		check{
			label:   "portal-contract-generate",
			command: tools.goCommand,
			args:    []string{"generate", "./internal/apicontract"},
		},
		check{
			label:   "portal-contract-diff",
			command: tools.gitCommand,
			args: []string{
				"diff", "--exit-code", "--",
				"portal/src/api/contract.generated.ts",
				"portal/src/api/wire.generated.ts",
			},
		},
		check{
			label:        "portal-contract-typecheck",
			command:      tools.npmCommand,
			args:         []string{"--prefix", "portal", "run", "typecheck"},
			windowsBatch: true,
		},
		check{
			label:        "portal-contract-test",
			command:      tools.npmCommand,
			args:         []string{"--prefix", "portal", "run", "test:contract"},
			windowsBatch: true,
		},
	)
	return result
}

func fullChecks(makeCommand string) []check {
	targets := []string{
		"ci",
		"test-integration-strict",
		"test-e2e",
		"test-envtest",
		"cover-check",
		"sandbox-check",
		"linux-node-validation",
		"test-shipped-workflows",
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
			current.label == "vet" ||
			strings.HasPrefix(current.label, "build-") {
			result = append(result, current)
		}
	}
	return result
}

func portalPreparationChecks(tools toolchain) []check {
	return []check{
		{
			label:        "portal-install",
			command:      tools.npmCommand,
			args:         []string{"--prefix", "portal", "ci", "--no-audit", "--no-fund"},
			windowsBatch: true,
		},
		{
			label:        "portal-build",
			command:      tools.npmCommand,
			args:         []string{"--prefix", "portal", "run", "build"},
			windowsBatch: true,
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
			_, _ = fmt.Fprintln(stdout, "These files are not gofmt-clean:")
			_, _ = stdout.Write(output)
			if output[len(output)-1] != '\n' {
				_, _ = fmt.Fprintln(stdout)
			}
			return fmt.Errorf("%s: files are not gofmt-clean", current.label)
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
