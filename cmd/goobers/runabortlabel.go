package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/terminalhooks/prepare"
	"github.com/goobers/goobers/internal/terminalreport"
	"github.com/goobers/goobers/providers"
)

// abortedRunLabel, prOpenOperation and runAbortLabelOperation are the names of
// prepare.AbortedRunLabel, prepare.PROpenOperation and
// prepare.RunAbortLabelOperation (see there for what they mean).
const (
	abortedRunLabel        = prepare.AbortedRunLabel
	prOpenOperation        = prepare.PROpenOperation
	runAbortLabelOperation = prepare.RunAbortLabelOperation
)

// prLabelFunc adds labels to a PR, modeled as a work item — the same shape
// every other label mutation in this codebase uses (UpdateWorkItem with a PR
// number as ID; see remediationcheckpoint.go / mergedemotion.go).
type prLabelFunc = prepare.PRLabelFunc

// workItemUpdater is the narrow seam buildTerminalRunAbortLabeler needs —
// just enough of providers.BacklogProvider to swap in a fake in tests,
// mirroring newTerminalBranchDeleter's providers.BranchDeleter seam.
type workItemUpdater interface {
	UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error)
}

var newRunAbortLabelProvider = func(source providers.TokenSource) workItemUpdater {
	return providers.NewGitHubProvider("", providers.WithTokenSource(source))
}

// newGiteaRunAbortLabelProvider is the Gitea arm of the same seam. It takes the
// resolved forge base URL because, unlike GitHub, a self-hosted Gitea has no
// well-known API host. Token resolution stays on the per-request TokenSource
// seam so the secret continues to flow through the run's registrar-based
// scrubbing rather than being copied into a second unregistered string.
var newGiteaRunAbortLabelProvider = func(baseURL string, source providers.TokenSource) workItemUpdater {
	return providers.NewGiteaProvider(baseURL, "", providers.WithGiteaTokenSource(source))
}

// newTerminalRunAbortLabelProviderForProject dispatches on the terminal
// repository's own declared provider kind. Labels are PR writes against that
// repository; ADO uses the native Git labels sub-endpoint, never Boards tags.
// Unknown providers fail rather than falling through to GitHub.
func newTerminalRunAbortLabelProviderForProject(cfg *instance.Config, project apiv1.RepoRef, source providers.TokenSource, registrar providers.SecretRegistrar, adoScheme string) (workItemUpdater, error) {
	return terminalProviderForProject(
		cfg,
		project,
		source,
		newRunAbortLabelProvider,
		newGiteaRunAbortLabelProvider,
		func(repo providers.RepositoryRef, kind string, source providers.TokenSource) (workItemUpdater, error) {
			return newADORunAbortLabelProvider(repo, kind, source, registrar)
		},
		adoScheme,
		func(kind providers.ProviderKind) error {
			return fmt.Errorf("run-abort labeling does not support repository provider %q", kind)
		},
	)
}

// buildTerminalRunAbortLabeler mirrors buildTerminalBranchDelete's shape: the
// same credential/capability wiring — including the per-gaggle project scoping
// (#2692 sibling), since the PR being labeled lives in the run gaggle's own
// repo — with provider:pr:write on ADO and the existing github:pr:write
// binding on GitHub/Gitea, since labeling a PR is a PR-write operation.
func buildTerminalRunAbortLabeler(cfg *instance.Config, project apiv1.RepoRef, registrar terminalSecretRegistry, stores credentials.StoreResolver) (prLabelFunc, error) {
	if len(cfg.Repos) == 0 {
		return nil, nil
	}
	gaggleOwner := project.Owner
	if project.Provider == apiv1.ProviderADO && project.Project != "" {
		gaggleOwner += "/" + project.Project
	}
	resolver, grants, err := runtimewiring.BuildCredentials(cfg, stores, gaggleOwner, project.Name, nil, registrar)
	if err != nil {
		return nil, err
	}
	injector, err := credentials.NewInjector(resolver, grants, registrar)
	if err != nil {
		return nil, fmt.Errorf("build run-abort label credentials: %w", err)
	}
	repo := terminalRepositoryRefForProject(cfg, project)
	required := string(capability.GitHubPRWrite)
	if repo.Provider == providers.ProviderADO {
		required = string(capability.ProviderPRWrite)
	}
	resolvedProject := apiv1.RepoRef{Provider: apiv1.Provider(repo.Provider), Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
	adoScheme := runtimewiring.AdoCredentialSchemes(cfg, resolvedProject, grants)[required]
	label := func(ctx context.Context, req providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
		set, err := injector.Materialize(ctx, []string{required})
		if err != nil {
			return providers.WorkItem{}, scrubTerminalError(registrar, err)
		}
		provider, err := newTerminalRunAbortLabelProviderForProject(cfg, project, set.For(required), registrar, adoScheme)
		if err != nil {
			return providers.WorkItem{}, scrubTerminalError(registrar, err)
		}
		result, err := provider.UpdateWorkItem(ctx, req)
		return result, scrubTerminalError(registrar, err)
	}
	return label, nil
}

// labelAbortedRunPR stamps abortedRunLabel on the PR this run opened, if any;
// the policy is prepare.LabelAbortedPR. Called on every terminal run, so it
// returns before any journal I/O for the overwhelmingly common completed case.
func labelAbortedRunPR(runsDir, runID string, phase journal.RunPhase, annotate terminalAnnotator, repo providers.RepositoryRef, labelPR prLabelFunc, reports ...*terminalreport.Queue) error {
	if labelPR == nil || !prepare.LabelsPhase(phase) {
		return nil
	}
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		return fmt.Errorf("open terminal run journal for run-abort label: %w", err)
	}
	events, err := rd.Events()
	if err != nil {
		return fmt.Errorf("read terminal run events for run-abort label: %w", err)
	}
	var outbox terminalreport.Outbox
	var gaggle string
	if len(reports) > 0 && reports[0] != nil {
		outbox, gaggle = reports[0], reports[0].Gaggle
	}
	return prepare.LabelAbortedPR(events, runID, annotate, repo, labelPR, outbox, gaggle)
}

func terminalFailureWasInfrastructure(events []journal.Event) bool {
	return prepare.FailureWasInfrastructure(events)
}
