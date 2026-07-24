package gooberruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// ExecutionEnvironment describes the prepared workspace handed to a harness.
type ExecutionEnvironment struct {
	WorkspaceDir string                `json:"workspaceDir"`
	RepoDir      string                `json:"repoDir"`
	Clone        providers.CloneResult `json:"clone"`
	Env          map[string]string     `json:"env,omitempty"`
}

// EnvironmentPreparer prepares an isolated workspace for a goober invocation.
type EnvironmentPreparer interface {
	Prepare(context.Context, apiv1.InvocationEnvelope) (ExecutionEnvironment, error)
}

// ProviderResolver resolves a repo provider for the invocation repository.
type ProviderResolver interface {
	RepoProvider(apiv1.Provider, apiv1.RepoRef) (providers.RepoProvider, error)
}

// StaticProviderResolver maps API provider names to in-process repo providers.
type StaticProviderResolver map[apiv1.Provider]providers.RepoProvider

// RepoProvider returns the configured repo provider for provider.
func (r StaticProviderResolver) RepoProvider(provider apiv1.Provider, _ apiv1.RepoRef) (providers.RepoProvider, error) {
	p, ok := r[provider]
	if !ok || p == nil {
		return nil, fmt.Errorf("repo provider %q is not configured", provider)
	}
	return p, nil
}

// EnvProviderResolver constructs providers from process environment variables.
type EnvProviderResolver struct {
	SecretRegistrar   providers.SecretRegistrar
	RateLimitObserver providers.RateLimitObserver
}

// RepoProvider returns a GitHub or ADO provider configured from env vars.
func (r EnvProviderResolver) RepoProvider(provider apiv1.Provider, repo apiv1.RepoRef) (providers.RepoProvider, error) {
	switch provider {
	case apiv1.ProviderGitHub:
		token := firstEnv("GOOBERS_GITHUB_TOKEN", "GITHUB_TOKEN")
		if token == "" {
			return nil, fmt.Errorf("github repo provider requires GOOBERS_GITHUB_TOKEN or GITHUB_TOKEN")
		}
		return providers.NewGitHubProvider(token, providers.WithRateLimitObserver(r.RateLimitObserver)), nil
	case apiv1.ProviderADO:
		token := firstEnv("GOOBERS_ADO_TOKEN", "AZURE_DEVOPS_TOKEN", "ADO_TOKEN")
		org, project := adoCoordinates(repo)
		if org == "" || project == "" {
			return nil, fmt.Errorf("ado repo provider requires organization and project")
		}
		if r.SecretRegistrar == nil {
			return nil, fmt.Errorf("ado repo provider requires a secret registrar")
		}
		authKind := firstEnv("GOOBERS_ADO_AUTH_KIND", "AZURE_DEVOPS_AUTH_KIND")
		if authKind == "" && token != "" {
			authKind = "pat"
		}
		var source providers.ADOCredentialSource
		var err error
		switch authKind {
		case "pat":
			if token == "" {
				return nil, fmt.Errorf("ado PAT auth requires GOOBERS_ADO_TOKEN, AZURE_DEVOPS_TOKEN, or ADO_TOKEN")
			}
			return providers.NewADOProvider(
				org,
				project,
				token,
				providers.WithADOSecretRegistrar(r.SecretRegistrar),
				providers.WithADORateLimitObserver(r.RateLimitObserver),
			), nil
		case "azure-cli":
			source = providers.NewAzureCLIADOCredentialSource(nil, firstEnv("GOOBERS_ADO_TENANT", "AZURE_DEVOPS_TENANT"))
		case "workload-identity":
			source, err = providers.NewWorkloadIdentityADOCredentialSource()
		case "managed-identity":
			source, err = providers.NewManagedIdentityADOCredentialSource(firstEnv("GOOBERS_ADO_CLIENT_ID", "AZURE_CLIENT_ID"))
		case "":
			return nil, fmt.Errorf("ado repo provider requires GOOBERS_ADO_AUTH_KIND or an ADO token")
		default:
			return nil, fmt.Errorf("ado repo provider has unsupported auth kind %q", authKind)
		}
		if err != nil {
			return nil, err
		}
		return providers.NewADOProvider(
			org,
			project,
			"",
			providers.WithADOCredentialSource(source),
			providers.WithADOSecretRegistrar(r.SecretRegistrar),
			providers.WithADORateLimitObserver(r.RateLimitObserver),
		), nil
	default:
		return nil, fmt.Errorf("unsupported repo provider %q", provider)
	}
}

// InProcessPreparer creates a local workspace and clones the target repo through
// the providers abstraction.
type InProcessPreparer struct {
	WorkspaceRoot string
	Providers     ProviderResolver
	Env           map[string]string
}

// Prepare creates a fresh workspace and clones the target repository into it.
func (p InProcessPreparer) Prepare(ctx context.Context, env apiv1.InvocationEnvelope) (ExecutionEnvironment, error) {
	if p.Providers == nil {
		return ExecutionEnvironment{}, fmt.Errorf("provider resolver is required")
	}
	root := p.WorkspaceRoot
	if root == "" {
		root = filepath.Join(os.TempDir(), "goobers-runs")
	}
	workspace := filepath.Join(root, safePathPart(env.RunID), safePathPart(env.TaskID))
	repoDir := filepath.Join(workspace, "repo")
	if err := os.RemoveAll(workspace); err != nil {
		return ExecutionEnvironment{}, fmt.Errorf("reset workspace %q: %w", workspace, err)
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return ExecutionEnvironment{}, fmt.Errorf("create workspace %q: %w", workspace, err)
	}

	repoProvider, err := p.Providers.RepoProvider(env.RepoRef.Provider, env.RepoRef)
	if err != nil {
		return ExecutionEnvironment{}, err
	}
	clone, err := repoProvider.CloneRepository(ctx, providers.CloneRequest{
		Repository:  toProviderRepo(env.RepoRef),
		Destination: repoDir,
		Branch:      env.RepoRef.Branch,
	})
	if err != nil {
		return ExecutionEnvironment{}, fmt.Errorf("clone repository: %w", err)
	}

	execEnv := ExecutionEnvironment{
		WorkspaceDir: workspace,
		RepoDir:      repoDir,
		Clone:        clone,
		Env:          p.runtimeEnv(env, workspace, repoDir),
	}
	return execEnv, nil
}

// KubernetesPreparer is the M8 pod-launch seam. The in-process implementation is
// active for v1 tests; operator-backed pod launch wires in here later.
type KubernetesPreparer struct{}

// Prepare returns a clear not-implemented error until the operator wires pod
// launch behind this interface.
func (KubernetesPreparer) Prepare(context.Context, apiv1.InvocationEnvelope) (ExecutionEnvironment, error) {
	return ExecutionEnvironment{}, fmt.Errorf("kubernetes goober pod launch is not implemented")
}

func (p InProcessPreparer) runtimeEnv(env apiv1.InvocationEnvelope, workspace, repoDir string) map[string]string {
	out := make(map[string]string, len(p.Env)+6)
	for k, v := range p.Env {
		out[k] = v
	}
	out["GOOBERS_RUN_ID"] = env.RunID
	out["GOOBERS_TASK_ID"] = env.TaskID
	out["GOOBERS_WORKFLOW_ID"] = env.WorkflowID
	out["GOOBERS_GAGGLE"] = env.Gaggle
	out["GOOBERS_WORKSPACE"] = workspace
	out["GOOBERS_REPO_DIR"] = repoDir
	return out
}

func toProviderRepo(repo apiv1.RepoRef) providers.RepositoryRef {
	owner := repo.Owner
	project := repo.Project
	if repo.Provider == apiv1.ProviderADO && project == "" {
		if org, parsedProject, ok := strings.Cut(repo.Owner, "/"); ok {
			owner = org
			project = parsedProject
		}
	}
	return providers.RepositoryRef{
		Provider: providers.ProviderKind(repo.Provider),
		Owner:    owner,
		Project:  project,
		Name:     repo.Name,
	}
}

func adoCoordinates(repo apiv1.RepoRef) (string, string) {
	org := firstEnv("GOOBERS_ADO_ORG", "AZURE_DEVOPS_ORG", "ADO_ORG")
	project := firstEnv("GOOBERS_ADO_PROJECT", "AZURE_DEVOPS_PROJECT", "ADO_PROJECT")
	if org == "" {
		org = repo.Owner
	}
	if project == "" {
		project = repo.Project
	}
	if parsedOrg, parsedProject, ok := strings.Cut(repo.Owner, "/"); ok {
		if org == repo.Owner {
			org = parsedOrg
		}
		if project == "" {
			project = parsedProject
		}
	}
	return org, project
}

func safePathPart(s string) string {
	replacer := strings.NewReplacer("/", "-", "\\", "-", ":", "-", "..", "-")
	out := strings.Trim(replacer.Replace(s), ". ")
	if out == "" {
		return "unknown"
	}
	return out
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}
