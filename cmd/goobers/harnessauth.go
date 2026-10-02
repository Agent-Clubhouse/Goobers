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
	"github.com/goobers/goobers/internal/bootstrap"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/workflow"
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

type copilotAuthCommander interface {
	AuthCommand(context.Context, string) ([]string, []string, error)
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
	return runHarnessAuthCopilotNative("logout", args, stdout, stderr)
}

func runHarnessAuthCopilotNative(operation string, args []string, stdout, stderr io.Writer) int {
	root, ok := parseHarnessAuthRoot(args, stderr, "harness auth copilot "+operation)
	if !ok {
		return 2
	}
	adapter, err := copilotAuthAdapter(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	commander, ok := adapter.(copilotAuthCommander)
	if !ok {
		pf(stderr, "error: configured Copilot adapter does not expose authentication %s\n", operation)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	command, env, err := commander.AuthCommand(ctx, operation)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := runCopilotNativeAuthCommand(ctx, command, env, stdout, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if operation == "login" {
		reporter, ok := adapter.(copilotAuthReporter)
		if !ok {
			pf(stderr, "error: configured Copilot adapter does not expose authentication status\n")
			return 1
		}
		info, err := reporter.AuthStatus(ctx)
		if err != nil {
			pf(stderr, "error: verify login: %v\n", err)
			return 1
		}
		printCopilotAuthInfo(stdout, info)
		if info.Status != harness.AuthStatusAuthenticated {
			pf(stderr, "error: login completed but Copilot authentication is %s\n", info.Status)
			return 1
		}
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
	if unavailable, err := copilotAuthPlacementUnavailable(root, cfg); err != nil {
		return nil, err
	} else if unavailable != "" {
		return nil, fmt.Errorf("interactive login unavailable in %s", unavailable)
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

func copilotAuthPlacementUnavailable(root string, cfg *instance.Config) (string, error) {
	configDir := layoutFor(root).ConfigDir()
	if _, err := os.Stat(configDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("inspect config directory: %w", err)
	}
	set, report, err := instance.LoadConfigDir(configDir)
	if err != nil {
		if summary := validationIssueSummary(report); summary != "" {
			return "", fmt.Errorf("load config directory: %w: %s", err, summary)
		}
		return "", fmt.Errorf("load config directory: %w", err)
	}
	goobers := goobersByName(set)
	gaggleRequiredCapabilities := make(map[string][]string, len(set.Gaggles))
	gaggleRunsOn := make(map[string]*apiv1.GaggleRunsOn, len(set.Gaggles))
	for i := range set.Gaggles {
		gaggleRequiredCapabilities[set.Gaggles[i].Name] = set.Gaggles[i].Spec.RequiredCapabilities
		gaggleRunsOn[set.Gaggles[i].Name] = set.Gaggles[i].Spec.RunsOn
	}
	for i := range set.Workflows {
		wf := set.Workflows[i]
		machine, err := workflow.Compile(
			workflow.Definition{Name: wf.Name, Version: 1, DSLVersion: wf.DSLVersion, Spec: wf.Spec, Annotations: wf.Annotations},
			workflow.WithGoobers(goobers),
			workflow.WithKnownChecks(knownAutomatedCheckNames()),
			workflow.WithKnownHarnesses([]string{string(apiv1.HarnessCopilot), string(apiv1.HarnessClaudeCode), string(apiv1.HarnessCodex)}),
			workflow.WithPreviewFeatures(workflow.PreviewFeaturesEnabled(wf.Annotations)),
			workflow.WithGaggleRequiredCapabilities(gaggleRequiredCapabilities[wf.Spec.Gaggle]),
			workflow.WithGaggleRunsOn(gaggleRunsOn[wf.Spec.Gaggle]),
		)
		if err != nil {
			return "", &workflowCompileError{Gaggle: wf.Spec.Gaggle, Workflow: wf.Name, Err: err}
		}
		placements, err := bootstrap.PinStagePlacements(cfg, set, wf.Spec.Gaggle, machine.Def)
		if err != nil {
			return "", err
		}
		if placement := copilotNonSelfPlacement(machine.Def.Spec, goobers, placements); placement != "" {
			return placement, nil
		}
	}
	return "", nil
}

func copilotNonSelfPlacement(spec apiv1.WorkflowSpec, goobers map[string]apiv1.GooberSpec, placements []engine.PinnedPlacement) string {
	byStage := make(map[string]engine.PinnedPlacement, len(placements))
	for _, placement := range placements {
		byStage[placement.Stage] = placement
	}
	for _, task := range spec.Tasks {
		if task.Type != apiv1.TaskAgentic || !gooberUsesCopilot(goobers, task.Goober) {
			continue
		}
		if placement := nonSelfPlacementDescription(byStage[task.Name]); placement != "" {
			return placement
		}
	}
	for _, gate := range spec.Gates {
		if gate.Evaluator != apiv1.EvaluatorAgentic || gate.Agentic == nil || !gooberUsesCopilot(goobers, gate.Agentic.Goober) {
			continue
		}
		if placement := nonSelfPlacementDescription(byStage[gate.Name]); placement != "" {
			return placement
		}
	}
	return ""
}

func gooberUsesCopilot(goobers map[string]apiv1.GooberSpec, name string) bool {
	spec, ok := goobers[name]
	if !ok {
		return false
	}
	return spec.Harness == "" || spec.Harness == apiv1.HarnessCopilot
}

func nonSelfPlacementDescription(placement engine.PinnedPlacement) string {
	if placement.Stage == "" || placement.Self {
		return ""
	}
	for _, runner := range placement.Eligible {
		if runner.HostKind == instance.RunnerHostSelf {
			continue
		}
		if strings.HasSuffix(placement.Queue, "."+runner.Name) {
			return fmt.Sprintf("%s runner %q", runner.HostKind, runner.Name)
		}
	}
	return "remote runner"
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

func runCopilotNativeAuthCommandDefault(ctx context.Context, command, env []string, stdout, stderr io.Writer) error {
	if len(command) == 0 {
		return fmt.Errorf("no Copilot command configured")
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(command, " "), err)
	}
	return nil
}
