package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/sandbox"
)

func (e *interactiveRestartExecution) agentic(name string, rec runner.ArtifactRecorder, reg runner.SecretRegistrar) (invoke.Goober, error) {
	spec, ok := e.goobers[name]
	if !ok {
		return nil, errors.New("interactive goober is outside pinned catalog")
	}
	registrar := teeRegistrar{run: reg, shared: e.setup.SharedRegistry}
	resolver := interactiveCredentialResolver{execution: e, goober: name, registrar: registrar}
	caps := map[string]string{}
	var grants []credentials.Grant
	for _, cap := range spec.Capabilities {
		_, kind, _ := interactiveCapability(cap)
		if kind != "" || cap == "agent:model" {
			caps[cap] = executor.CredentialEnvVar(cap)
			grants = append(grants, credentials.Grant{Goober: name, Capability: cap, Ref: cap})
		}
	}
	registry, err := buildHarnessRegistry(caps, harness.EnvironmentConfig{}, nil, "", "", true, nil, false)
	if err != nil {
		return nil, err
	}
	adapter, err := registry.Get(string(spec.Harness))
	if err != nil {
		return nil, err
	}
	switch a := adapter.(type) {
	case *harness.ClaudeAdapter:
		a.OptionalCredentialCapabilities = nil
	case *harness.CodexAdapter:
		a.OptionalCredentialCapabilities = nil
	}
	restricted := harness.NewRegistry()
	if err = restricted.RegisterAs(string(spec.Harness), interactiveRestartAdapter{Adapter: adapter, execution: e}); err != nil {
		return nil, err
	}
	instructions := map[string]string{name: e.instructions[name] + "\n" + interactiveCredentialInstructions()}
	delegate, err := buildAgenticExecutor(agenticExecutorInput{GooberName: name, Goobers: e.goobers, Instructions: instructions, Assets: e.assets, SkillPackages: e.skills, AdapterRegistry: restricted, EnvCapabilities: caps, Resolver: resolver, Grants: grants, SharedRegistry: e.setup.SharedRegistry, RunsDir: e.layout.RunsDir(), SandboxPosture: instance.SandboxEnforced, ArtifactRecorder: rec, SecretRegistrar: reg, GuardedCredentialPaths: instance.GuardedCredentialPaths(e.setup.Config)})
	if err != nil {
		return nil, err
	}
	return interactiveRestartGoober{Goober: delegate}, nil
}

type interactiveRestartGoober struct{ invoke.Goober }

func (g interactiveRestartGoober) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	ctx, release, err := interactiveOperationContext(ctx)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer release()
	return g.Goober.Invoke(context.WithValue(ctx, interactiveRepositoryKey{}, env.RepoRef), env)
}
func (g interactiveRestartGoober) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	ctx, release, err := interactiveOperationContext(ctx)
	if err != nil {
		return apiv1.Verdict{}, err
	}
	defer release()
	return g.Goober.Review(context.WithValue(ctx, interactiveRepositoryKey{}, env.RepoRef), env)
}

type interactiveRestartAdapter struct {
	harness.Adapter
	execution *interactiveRestartExecution
}

func (a interactiveRestartAdapter) Run(ctx context.Context, req harness.RunRequest) (harness.Outcome, error) {
	runtime, err := interactiveRuntime(ctx)
	if err != nil {
		return harness.Outcome{}, err
	}
	if req.Sandbox == nil {
		return harness.Outcome{}, errors.New("interactive restart sandbox unavailable")
	}
	if runtime.execution != a.execution || req.ChildWorkflows != nil || len(req.MCPServers) > 0 {
		return harness.Outcome{}, errors.New("interactive restart agent authority mismatch")
	}
	home, err := os.MkdirTemp(runtime.home, "agent-")
	if err != nil {
		return harness.Outcome{}, err
	}
	defer func() { _ = os.RemoveAll(home) }()
	req.IsolatedHome = home
	req.CredentialAudiences = map[string]apiv1.Provider{}
	for _, cap := range req.Envelope.Capabilities {
		_, kind, _ := interactiveCapability(cap)
		if kind == "backlog" {
			req.CredentialAudiences[cap] = a.execution.gaggle.Spec.Backlog.Provider
		}
	}
	req.Sandbox = interactiveRestartSandbox{Sandbox: req.Sandbox, home: home, denied: interactiveAmbientAuthPaths()}
	out, runErr := a.Adapter.Run(ctx, req)
	if runtime.proof != nil {
		runErr = errors.Join(runErr, runtime.proof.VerifyIdle())
	}
	return out, runErr
}

type interactiveRestartSandbox struct {
	sandbox.Sandbox
	home   string
	denied []string
}

func (s interactiveRestartSandbox) Wrap(command *exec.Cmd, policy sandbox.Policy) error {
	policy.WritableRoots = append(policy.WritableRoots, s.home)
	policy.ReadDeniedPaths = append(policy.ReadDeniedPaths, s.denied...)
	return s.Sandbox.Wrap(command, policy)
}

func interactiveAmbientAuthPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return []string{"/"}
	} // invalid policy fails closed
	paths := []string{}
	for _, path := range []string{".ssh", ".git-credentials", ".gitconfig", ".netrc", ".npmrc", ".pypirc", ".aws", ".azure", ".config/gh", ".config/gcloud", ".docker", ".kube", ".codex", ".claude"} {
		absolute := filepath.Join(home, path)
		if _, err := os.Lstat(absolute); err == nil {
			paths = append(paths, absolute)
		} else if !os.IsNotExist(err) {
			return []string{"/"}
		}
	}
	for _, key := range []string{"GH_CONFIG_DIR", "AZURE_CONFIG_DIR", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "KUBECONFIG", "DOCKER_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "SSH_AUTH_SOCK"} {
		value := os.Getenv(key)
		if value == "" {
			continue
		}
		if !filepath.IsAbs(value) {
			return []string{"/"}
		}
		if _, err := os.Lstat(value); err == nil {
			paths = append(paths, value)
		} else if !os.IsNotExist(err) {
			return []string{"/"}
		}
	}
	return paths
}
