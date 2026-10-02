package runtimeplan

import (
	"fmt"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credreadiness"
	"github.com/goobers/goobers/internal/instance"
)

// Inputs are the declared runtime settings. Dynamic workspace allocations and
// cancellation of a live process cannot be proven by inspecting configuration.
// Only source kinds are retained for credentials, never refs or resolved values.
type Inputs struct {
	Paths             []Path             `json:"paths"`
	CredentialSources []CredentialSource `json:"credentialSources"`
	Sandbox           Sandbox            `json:"sandbox"`
	Timeouts          Timeouts           `json:"timeouts"`
	Source            Source             `json:"source"`
}
type Path struct {
	Purpose string `json:"purpose"`
	Path    string `json:"path"`
	Source  Source `json:"source"`
}
type CredentialSource struct {
	Scope      string `json:"scope"`
	Capability string `json:"capability,omitempty"`
	Harness    string `json:"harness,omitempty"`
	Kind       string `json:"kind"`
}
type Sandbox struct {
	Agentic     string                    `json:"agentic"`
	Isolation   *instance.IsolationConfig `json:"isolation,omitempty"`
	Runners     []RunnerIsolation         `json:"runners,omitempty"`
	Enforcement Identity                  `json:"enforcement"`
}
type RunnerIsolation struct {
	Name         string                       `json:"name"`
	Restrictions []instance.RunnerRestriction `json:"restrictions,omitempty"`
}
type Timeouts struct {
	RunnerDefault      string                 `json:"runnerDefault,omitempty"`
	RequiredMCPSettle  string                 `json:"requiredMCPSettle,omitempty"`
	RunConditions      instance.RunConditions `json:"runConditions"`
	RepositoryDefaults []string               `json:"repositoryDefaults,omitempty"`
	Cancellation       Identity               `json:"cancellation"`
}

func ResolveInputs(layout instance.Layout, cfg *instance.Config, gaggle apiv1.GaggleSpec) Inputs {
	static := Source{"static", "loaded instance and workflow configuration; no path access or runtime enforcement probe"}
	// The caller supplies an already gaggle-scoped layout. Keep that scope when
	// applying the same workcopy override used by execution.
	scoped := layout
	if cfg.Workcopies != nil && cfg.Workcopies.Root != "" {
		scoped = scoped.WithWorkcopiesRoot(cfg.Workcopies.Root)
	}
	result := Inputs{Source: static, CredentialSources: []CredentialSource{}, Sandbox: Sandbox{
		Agentic: string(instance.EffectiveAgenticSandbox(cfg, &apiv1.Gaggle{Spec: gaggle})), Isolation: cfg.Isolation,
		Enforcement: Identity{"unobservable", "sandbox_enforcement_unobservable", "configured posture is not proof of enforcement on the target runner", Source{"unobservable", "target runner not probed"}},
	}, Timeouts: Timeouts{RunnerDefault: cfg.Runner.DefaultStageTimeout, RequiredMCPSettle: cfg.Runner.RequiredMCPSettleTimeout, RunConditions: cfg.RunConditions,
		Cancellation: Identity{"unobservable", "cancellation_unobservable", "attempt parent deadlines and process cleanup require target execution", Source{"unobservable", "no running attempt"}}}}
	for _, entry := range []struct{ purpose, path string }{{"instance", layout.Root}, {"config", layout.ConfigDir()}, {"runs", scoped.RunsDir()}, {"workcopies", scoped.WorkcopiesDir()}, {"configMirror", cfg.ConfigMirrorPath}} {
		if entry.path != "" {
			result.Paths = append(result.Paths, Path{entry.purpose, entry.path, static})
		}
	}
	for i, repo := range cfg.Repos {
		kind, _ := credreadiness.Describe(repo.Token.CredentialTokenRef(""))
		k := string(kind)
		if repo.Auth != nil {
			k = repo.Auth.Kind
		}
		result.CredentialSources = append(result.CredentialSources, CredentialSource{Scope: fmt.Sprintf("repos[%d]", i), Kind: k})
		result.Timeouts.RepositoryDefaults = append(result.Timeouts.RepositoryDefaults, repo.EffectiveDefaultStageTimeout(cfg.Runner.DefaultStageTimeout))
	}
	for _, grant := range cfg.Credentials {
		kind, _ := credreadiness.Describe(grant.Token.CredentialTokenRef(""))
		if grant.GitHubApp != nil {
			kind = credreadiness.SourceGitHubApp
		}
		scope := "capability"
		if grant.MCP != "" {
			scope = "mcp"
		}
		result.CredentialSources = append(result.CredentialSources, CredentialSource{scope, grant.Capability, grant.Harness, string(kind)})
	}
	if cfg.DaemonIdentity != nil {
		kind := credreadiness.SourceUnsupported
		if cfg.DaemonIdentity.Token != nil {
			kind, _ = credreadiness.Describe(cfg.DaemonIdentity.Token.CredentialTokenRef(""))
		}
		if cfg.DaemonIdentity.GitHubApp() {
			kind = credreadiness.SourceGitHubApp
		}
		result.CredentialSources = append(result.CredentialSources, CredentialSource{Scope: "daemonIdentity", Kind: string(kind)})
	}
	for _, runner := range cfg.ResolvedRunners() {
		result.Sandbox.Runners = append(result.Sandbox.Runners, RunnerIsolation{runner.Name, runner.Restrictions})
	}
	sort.Slice(result.Sandbox.Runners, func(i, j int) bool { return result.Sandbox.Runners[i].Name < result.Sandbox.Runners[j].Name })
	return result
}
