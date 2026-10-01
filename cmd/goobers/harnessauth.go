package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/secretstore"
)

const harnessAuthHelp = "Usage: goobers harness auth copilot status [path]\n" +
	"       goobers harness auth copilot login [path]\n" +
	"       goobers harness auth copilot logout [path]\n\n" +
	"Inspect or delegate Copilot harness authentication using the same configured\n" +
	"command, environment policy, model-credential precedence, and launcher\n" +
	"preflight path used before agentic stages. Status output is credential-free:\n" +
	"it reports authenticated, signed-out, or unknown with the selected executable,\n" +
	"version when available, runner, and profile directory. Login delegates to the\n" +
	"native Copilot CLI login flow. Logout reports unsupported for current direct\n" +
	"Copilot CLI installations instead of clearing the wrong profile.\n"

var runCopilotNativeAuthCommand = runCopilotNativeAuthCommandDefault

type copilotAuthReporter interface {
	AuthStatus(context.Context) (harness.AuthInfo, error)
}

func runHarness(args []string, stdout, stderr io.Writer) int {
	pf(stderr, "%s", harnessAuthHelp)
	return 2
}

func runHarnessAuth(args []string, stdout, stderr io.Writer) int {
	pf(stderr, "%s", harnessAuthHelp)
	return 2
}

func runHarnessAuthCopilot(args []string, stdout, stderr io.Writer) int {
	pf(stderr, "%s", harnessAuthHelp)
	return 2
}

func runHarnessAuthCopilotStatus(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("harness auth copilot status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "harness auth copilot status")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := singleOptionalRoot(fs.Args(), stderr, "harness auth copilot status")
	if !ok {
		return 2
	}
	adapter, err := copilotAuthAdapter(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	reporter, ok := adapter.(copilotAuthReporter)
	if !ok {
		pf(stderr, "error: configured Copilot adapter does not expose authentication status\n")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), harnessPreflightTimeout)
	defer cancel()
	info, err := reporter.AuthStatus(ctx)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	printCopilotAuthInfo(stdout, info)
	if info.Status == harness.AuthStatusAuthenticated {
		return 0
	}
	return 1
}

func runHarnessAuthCopilotLogin(args []string, stdout, stderr io.Writer) int {
	return runHarnessAuthCopilotNative("login", args, stdout, stderr)
}

func runHarnessAuthCopilotLogout(args []string, stdout, stderr io.Writer) int {
	root, ok := parseHarnessAuthRoot(args, stderr, "harness auth copilot logout")
	if !ok {
		return 2
	}
	_, _ = root, stdout
	pf(stderr, "error: Copilot CLI logout is not supported by this native harness; clear credentials with the vendor-supported mechanism for the selected profile\n")
	return 1
}

func runHarnessAuthCopilotNative(operation string, args []string, stdout, stderr io.Writer) int {
	root, ok := parseHarnessAuthRoot(args, stderr, "harness auth copilot "+operation)
	if !ok {
		return 2
	}
	cfg, err := loadHarnessAuthConfig(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	command := harnessCommandOrDefault(cfg.Runner.HarnessCommand, string(apiv1.HarnessCopilot), []string{"copilot"})
	command = append(append([]string(nil), command...), operation)
	if err := runCopilotNativeAuthCommand(context.Background(), command, harnessEnvironmentPolicy(cfg.Runner), stdout, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func parseHarnessAuthRoot(args []string, stderr io.Writer, command string) (string, bool) {
	fs := newCLIFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, command)
	if err := fs.Parse(args); err != nil {
		return "", false
	}
	return singleOptionalRoot(fs.Args(), stderr, command)
}

func singleOptionalRoot(args []string, stderr io.Writer, command string) (string, bool) {
	switch len(args) {
	case 0:
		return ".", true
	case 1:
		return args[0], true
	default:
		pf(stderr, "error: %s accepts at most one instance root\n", command)
		return "", false
	}
}

func copilotAuthAdapter(root string) (harness.Adapter, error) {
	cfg, err := loadHarnessAuthConfig(root)
	if err != nil {
		return nil, err
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return nil, fmt.Errorf("load secret stores: %w", err)
	}
	modelCredential, _, err := agentModelCredentialResolver(cfg, stores, apiv1.HarnessCopilot)
	if err != nil {
		return nil, fmt.Errorf("resolve agent:model credential: %w", err)
	}
	return harnessAdapterFor(apiv1.HarnessCopilot, harnessEnvironmentPolicy(cfg.Runner), cfg.Runner.HarnessCommand, modelCredential)
}

func loadHarnessAuthConfig(root string) (*instance.Config, error) {
	path := layoutFor(root).ConfigFile()
	cfg, err := instance.LoadConfig(path)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return &instance.Config{}, nil
	}
	return nil, fmt.Errorf("load instance config: %w", err)
}

func printCopilotAuthInfo(stdout io.Writer, info harness.AuthInfo) {
	pf(stdout, "HARNESS copilot auth: %s\n", info.Status)
	if info.Executable != "" {
		pf(stdout, "  executable: %s\n", info.Executable)
	}
	if info.Version != "" {
		pf(stdout, "  version: %s\n", info.Version)
	}
	if info.Runner != "" {
		pf(stdout, "  runner: %s\n", info.Runner)
	}
	if info.ProfileDir != "" {
		pf(stdout, "  profile: %s\n", info.ProfileDir)
	}
	if info.Remediation != "" {
		pf(stdout, "  remediation: %s\n", info.Remediation)
	}
}

func runCopilotNativeAuthCommandDefault(ctx context.Context, command []string, environment harness.EnvironmentConfig, stdout, stderr io.Writer) error {
	if len(command) == 0 {
		return fmt.Errorf("no Copilot command configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = harness.AuthEnvironment(environment)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(command, " "), err)
	}
	return nil
}
