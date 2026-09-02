package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

var guidedDiscoveryCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

var guidedGitHubAuthorizationCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

var guidedGitHubAuthorizationRunner = func(ctx context.Context) error {
	command := guidedGitHubAuthorizationCommand(
		ctx,
		"gh",
		"auth",
		"login",
		"--hostname",
		"github.com",
		"--git-protocol",
		"https",
		"--web",
	)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

const guidedGitHubLoginCommand = "gh auth login --hostname github.com --git-protocol https --web"

type guidedRepositoryInspection struct {
	Provider             string          `json:"provider"`
	Owner                string          `json:"owner"`
	Project              string          `json:"project,omitempty"`
	Name                 string          `json:"name"`
	DisplayName          string          `json:"displayName"`
	GaggleName           string          `json:"gaggleName"`
	LocalPath            string          `json:"localPath,omitempty"`
	DefaultBranch        string          `json:"defaultBranch"`
	Stack                string          `json:"stack,omitempty"`
	CICommand            []string        `json:"ciCommand,omitempty"`
	RequiredCapabilities []string        `json:"requiredCapabilities,omitempty"`
	PullRequestCI        bool            `json:"pullRequestCI,omitempty"`
	Discovery            string          `json:"discovery"`
	Evidence             []string        `json:"evidence,omitempty"`
	NeedsClone           bool            `json:"needsClone"`
	PeerInstancePath     string          `json:"peerInstancePath,omitempty"`
	Ephemeral            bool            `json:"ephemeral,omitempty"`
	EphemeralReason      string          `json:"ephemeralReason,omitempty"`
	SafeInstancePath     string          `json:"safeInstancePath,omitempty"`
	Auth                 guidedAuthState `json:"auth"`
}

type guidedAuthState struct {
	Kind               string `json:"kind"`
	Ready              bool   `json:"ready"`
	Identity           string `json:"identity,omitempty"`
	RemediationCommand string `json:"remediationCommand,omitempty"`
	Message            string `json:"message,omitempty"`
	NeedsLogin         bool   `json:"needsLogin,omitempty"`
}

func inspectGuidedRepository(ctx context.Context, input string) (guidedRepositoryInspection, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return guidedRepositoryInspection{}, errors.New("repository location is required")
	}
	if info, err := os.Stat(input); err == nil && info.IsDir() {
		return inspectGuidedLocalRepository(ctx, input)
	}
	identity, err := parseGuidedRepositoryIdentity(input)
	if err != nil {
		return guidedRepositoryInspection{}, fmt.Errorf("repository must be an existing local Git clone or a GitHub/Azure DevOps repository URL: %w", err)
	}
	inspection := guidedInspectionFromIdentity(identity)
	inspection.NeedsClone = true
	inspection.Discovery = "provider-metadata"
	inspection.DefaultBranch = discoverRemoteDefaultBranch(ctx, identity)
	inspection.Auth = guidedAuthDiscovery(ctx, identity)
	return inspection, nil
}

func inspectGuidedLocalRepository(ctx context.Context, input string) (guidedRepositoryInspection, error) {
	absolute, err := filepath.Abs(input)
	if err != nil {
		return guidedRepositoryInspection{}, fmt.Errorf("resolve repository path: %w", err)
	}
	root, err := runGuidedDiscovery(ctx, "git", "-C", absolute, "rev-parse", "--show-toplevel")
	if err != nil {
		return guidedRepositoryInspection{}, fmt.Errorf("%s is not a Git repository: %w", absolute, err)
	}
	root, err = filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return guidedRepositoryInspection{}, fmt.Errorf("resolve Git root: %w", err)
	}
	remote, err := guidedRepositoryRemote(ctx, root)
	if err != nil {
		return guidedRepositoryInspection{}, err
	}
	identity, err := parseGuidedRepositoryIdentity(remote)
	if err != nil {
		return guidedRepositoryInspection{}, fmt.Errorf("the repository remote is not GitHub or Azure DevOps: %w", err)
	}
	inspection := guidedInspectionFromIdentity(identity)
	inspection.LocalPath = root
	inspection.PeerInstancePath = filepath.Join(filepath.Dir(root), filepath.Base(root)+"-goobers")
	if safety, safetyErr := worktree.InspectInitTarget(ctx, root); safetyErr == nil && safety.Ephemeral {
		inspection.Ephemeral = true
		inspection.EphemeralReason = safety.Reason
		inspection.SafeInstancePath = worktree.RecommendedInstancePath(safety)
		inspection.PeerInstancePath = inspection.SafeInstancePath
	}
	inspection.DefaultBranch, err = discoverLocalDefaultBranch(ctx, root)
	if err != nil {
		return guidedRepositoryInspection{}, err
	}
	inspection.Auth = guidedAuthDiscovery(ctx, identity)

	stack, command, capability := detectCICommandDefault(root)
	if len(command) > 0 && capability != "" {
		inspection.Stack = stack
		inspection.CICommand = command
		inspection.RequiredCapabilities = []string{capability}
		inspection.Discovery = "deterministic"
		inspection.Evidence = []string{"Detected from repository build manifests."}
		return inspection, nil
	}
	inspection.PullRequestCI = true
	inspection.Discovery = "unresolved"
	inspection.Evidence = []string{"No authoritative local CI command was found in root build or guidance files; provider CI will run after the pull request opens."}
	return inspection, nil
}

type guidedRepositoryIdentity struct {
	provider string
	owner    string
	project  string
	name     string
}

func parseGuidedRepositoryIdentity(value string) (guidedRepositoryIdentity, error) {
	if ado, ok := connectADOIdentity(value); ok {
		return guidedRepositoryIdentity{
			provider: "ado",
			owner:    ado.Organization,
			project:  ado.Project,
			name:     ado.Repository,
		}, nil
	}
	owner, name, err := parseGitHubRepo(value)
	if err != nil {
		return guidedRepositoryIdentity{}, err
	}
	return guidedRepositoryIdentity{provider: "github", owner: owner, name: name}, nil
}

func guidedInspectionFromIdentity(identity guidedRepositoryIdentity) guidedRepositoryInspection {
	display := identity.owner + "/" + identity.name
	if identity.provider == string(providers.ProviderADO) {
		display = identity.owner + "/" + identity.project + "/" + identity.name
	}
	return guidedRepositoryInspection{
		Provider:    identity.provider,
		Owner:       identity.owner,
		Project:     identity.project,
		Name:        identity.name,
		DisplayName: display,
		GaggleName:  guidedGaggleName(identity.name),
	}
}

func guidedRepositoryRemote(ctx context.Context, root string) (string, error) {
	remotes, err := runGuidedDiscovery(ctx, "git", "-C", root, "remote")
	if err != nil {
		return "", fmt.Errorf("list repository remotes: %w", err)
	}
	names := strings.Fields(remotes)
	if len(names) == 0 {
		return "", errors.New("the local repository has no Git remote; add a GitHub or Azure DevOps remote first")
	}
	selected := names[0]
	for _, name := range names {
		if name == "origin" {
			selected = name
			break
		}
	}
	remote, err := runGuidedDiscovery(ctx, "git", "-C", root, "remote", "get-url", selected)
	if err != nil {
		return "", fmt.Errorf("read repository remote: %w", err)
	}
	return strings.TrimSpace(remote), nil
}

func discoverLocalDefaultBranch(ctx context.Context, root string) (string, error) {
	if branch, err := runGuidedDiscovery(ctx, "git", "-C", root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if _, value, ok := strings.Cut(strings.TrimSpace(branch), "/"); ok && value != "" {
			return value, nil
		}
	}
	if branch, err := runGuidedDiscovery(ctx, "git", "-C", root, "branch", "--show-current"); err == nil && strings.TrimSpace(branch) != "" {
		return strings.TrimSpace(branch), nil
	}
	return "", errors.New("could not determine the repository default branch; set origin/HEAD or check out the intended default branch, then inspect again")
}

func discoverRemoteDefaultBranch(ctx context.Context, identity guidedRepositoryIdentity) string {
	switch identity.provider {
	case "github":
		repository := identity.owner + "/" + identity.name
		if branch, err := runGuidedDiscovery(ctx, "gh", "repo", "view", repository, "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name"); err == nil && strings.TrimSpace(branch) != "" {
			return strings.TrimSpace(branch)
		}
	case "ado":
		organization := "https://dev.azure.com/" + url.PathEscape(identity.owner)
		if branch, err := runGuidedDiscovery(
			ctx,
			"az",
			"repos",
			"show",
			"--organization",
			organization,
			"--project",
			identity.project,
			"--repository",
			identity.name,
			"--query",
			"defaultBranch",
			"--output",
			"tsv",
		); err == nil {
			return strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
		}
	}
	return ""
}

func discoverGuidedAuth(ctx context.Context, identity guidedRepositoryIdentity) guidedAuthState {
	switch identity.provider {
	case "github":
		login, err := runGuidedDiscovery(ctx, "gh", "api", "user", "--jq", ".login")
		login = strings.TrimSpace(login)
		if err != nil || login == "" {
			return guidedAuthState{
				Kind:               "github-cli",
				RemediationCommand: guidedGitHubLoginCommand,
				Message:            "GitHub CLI authentication is required before setup can continue.",
				NeedsLogin:         true,
			}
		}

		_, err = runGuidedDiscovery(ctx, "gh", "repo", "view", identity.owner+"/"+identity.name, "--json", "name", "--jq", ".name")
		if err != nil {
			return guidedAuthState{
				Kind:               "github-cli",
				Identity:           login,
				RemediationCommand: "gh repo view " + identity.owner + "/" + identity.name,
				Message: fmt.Sprintf(
					"GitHub CLI is authenticated as %s, but that account cannot access %s.",
					login,
					identity.owner+"/"+identity.name,
				),
			}
		}

		return guidedAuthState{
			Kind:     "github-cli",
			Ready:    true,
			Identity: login,
			Message:  "GitHub CLI authentication and repository access are ready.",
		}
	case "ado":
		login, err := runGuidedDiscovery(ctx, "az", "account", "show", "--query", "user.name", "--output", "tsv")
		login = strings.TrimSpace(login)
		if err != nil || login == "" {
			return guidedAuthState{
				Kind:               "azure-cli",
				RemediationCommand: "az login",
				Message:            "Azure CLI authentication is required before setup can continue.",
				NeedsLogin:         true,
			}
		}

		token, err := runGuidedDiscovery(
			ctx,
			"az",
			"account",
			"get-access-token",
			"--resource",
			"499b84ac-1321-427f-aa17-267ca6975798",
			"--query",
			"expiresOn",
			"--output",
			"tsv",
		)
		if err != nil || strings.TrimSpace(token) == "" {
			return guidedAuthState{
				Kind:     "azure-cli",
				Identity: login,
				RemediationCommand: fmt.Sprintf(
					"az repos show --organization https://dev.azure.com/%s --project %q --repository %q",
					url.PathEscape(identity.owner),
					identity.project,
					identity.name,
				),
				Message: fmt.Sprintf(
					"Azure CLI is authenticated as %s, but Azure DevOps access to %s could not be verified.",
					login,
					identity.owner+"/"+identity.project+"/"+identity.name,
				),
			}
		}

		return guidedAuthState{
			Kind:     "azure-cli",
			Ready:    true,
			Identity: login,
			Message:  "Azure CLI authentication and repository access are ready.",
		}
	default:
		return guidedAuthState{}
	}
}

var guidedAuthDiscovery = discoverGuidedAuth

func runGuidedDiscovery(ctx context.Context, name string, args ...string) (string, error) {
	cmd := guidedDiscoveryCommand(ctx, name, args...)
	configureGuidedDiscoveryCommand(cmd, name)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", errors.New(detail)
	}
	return string(output), nil
}

func configureGuidedDiscoveryCommand(cmd *exec.Cmd, name string) {
	if name == "gh" && os.Getenv("GH_TOKEN") == "" {
		if token := os.Getenv("GOOBERS_GITHUB_TOKEN"); token != "" {
			cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
		}
	}
}

func runGuidedGitHubAuthorization(ctx context.Context) error {
	if err := guidedGitHubAuthorizationRunner(ctx); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("GitHub CLI (`gh`) is not installed or not available on PATH")
		}
		return fmt.Errorf("GitHub device/web authorization did not complete: %w", err)
	}
	return nil
}
