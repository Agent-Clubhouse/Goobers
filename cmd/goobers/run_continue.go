package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/providers"
)

const runContinueHelp = "Usage: goobers run continue --from <run-id> --terminal-seq <seq> --target <state> --operator <id> [path]\n\n" +
	"Create a distinct continuation journal from a terminal run. The source\n" +
	"journal is never modified and no workflow stages are executed.\n"

func runRunContinue(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("run continue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "terminal source run id")
	terminalSeq := fs.Uint64("terminal-seq", 0, "source run.finished sequence")
	target := fs.String("target", "", "requested workflow state")
	operator := fs.String("operator", "", "operator identity")
	branch := fs.String("branch", "", "source branch to verify and reuse")
	expectedSHA := fs.String("expected-sha", "", "expected source branch head commit")
	integrity := fs.String("integrity", string(apiv1.IntegrityUnapproved), "integrity grade for injected inputs")
	var inputFlags stringListFlag
	fs.Var(&inputFlags, "input", "injected input as name=path (repeatable)")
	var contextFlags stringListFlag
	fs.Var(&contextFlags, "context", "prior artifact pointer name to carry (repeatable)")
	fs.Usage = func() { pf(stderr, "%s", runContinueHelp) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *from == "" || *terminalSeq == 0 || *target == "" || *operator == "" ||
		fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	sourceDir, err := instance.NewLayout(root).FindRunDir(*from)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	sourceID := filepath.Base(sourceDir)
	sourceReader, err := journal.OpenReadOnly(sourceDir)
	if err != nil {
		pf(stderr, "error: inspect continuation source: %v\n", err)
		return 1
	}
	sourceIdentity, err := sourceReader.Identity()
	if err != nil {
		pf(stderr, "error: read continuation source identity: %v\n", err)
		return 1
	}
	pinnedMachine, err := runner.PinnedWorkflowMachine(sourceReader, sourceIdentity)
	if err != nil {
		pf(stderr, "error: resolve continuation source workflow: %v\n", err)
		return 1
	}
	candidateMachine, err := currentWorkflowMachine(root, sourceIdentity)
	if err != nil {
		pf(stderr, "error: resolve continuation candidate workflow: %v\n", err)
		return 1
	}
	if err := runner.ValidateContinuationTarget(pinnedMachine, candidateMachine, *target); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	sourceBranch := strings.TrimSpace(*branch)
	if sourceBranch == "" {
		sourceBranch = strings.TrimSpace(sourceIdentity.WorkspaceBranch)
	}
	sourceSHA := strings.TrimSpace(*expectedSHA)
	if sourceSHA == "" {
		sourceSHA = strings.TrimSpace(sourceIdentity.WorkspaceBranchSHA)
	}

	// Extract branch from recorded events if not provided directly.
	// This matches CreateContinuation's logic: branch provenance in EventRefTouched
	// is the source of truth when WorkspaceBranch is empty.
	var recordedBranch, recordedSHA string
	sourceEvents, err := sourceReader.Events()
	if err != nil {
		pf(stderr, "error: read continuation source events: %v\n", err)
		return 1
	}
	for _, event := range sourceEvents {
		if event.Type == journal.EventRefTouched && event.ExternalRef != nil && event.ExternalRef.Kind == "branch" {
			recordedBranch = event.ExternalRef.ID
			recordedSHA = event.ExternalRef.CommitSHA
		}
	}
	if sourceBranch == "" {
		sourceBranch = recordedBranch
	}
	if sourceSHA == "" {
		sourceSHA = recordedSHA
	}

	repo, err := continuationRepository(root, sourceIdentity.Gaggle)
	if err != nil {
		pf(stderr, "error: resolve continuation repository: %v\n", err)
		return 1
	}

	// Always validate repository identity and namespace independently of
	// whether a branch is being reused. Repository identity should be verified
	// when it's recorded, regardless of whether branch provenance comes from
	// the direct request, WorkspaceBranch, or EventRefTouched events.
	if sourceIdentity.WorkspaceRepository != nil {
		if !sameContinuationRepository(*sourceIdentity.WorkspaceRepository, repo) {
			pf(stderr, "error: continuation source repository does not match configured gaggle repository\n")
			return 1
		}
	}
	if sourceBranch != "" {
		namespace, namespaceErr := continuationBranchNamespace(root, sourceIdentity.Gaggle)
		if namespaceErr != nil {
			pf(stderr, "error: resolve continuation branch namespace: %v\n", namespaceErr)
			return 1
		}
		if !strings.HasPrefix(sourceBranch, namespace) {
			pf(stderr, "error: continuation branch %q is outside gaggle namespace %q\n", sourceBranch, namespace)
			return 1
		}
	}
	provider, err := newProviderForStage(root, repo, true)
	if err != nil {
		pf(stderr, "error: resolve continuation provider: %v\n", err)
		return 1
	}
	branchProvider, ok := provider.(providers.BranchReconciliationProvider)
	if !ok {
		pf(stderr, "error: provider %q cannot verify branches\n", repo.Provider)
		return 1
	}
	contextPointers, err := selectedContinuationPointers(sourceIdentity.ContextPointers, contextFlags, sourceID)
	if err != nil {
		pf(stderr, "error: select continuation context: %v\n", err)
		return 2
	}
	inputs := make(map[string][]byte, len(inputFlags))
	inputSources := make(map[string]string, len(inputFlags))
	for _, value := range inputFlags {
		name, path, ok := strings.Cut(value, "=")
		if !ok || name == "" || path == "" {
			pf(stderr, "error: invalid --input %q; expected name=path\n", value)
			return 2
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			pf(stderr, "error: read injected input %q: %v\n", name, readErr)
			return 2
		}
		inputs[name] = data
		inputSources[name] = path
	}
	grade := apiv1.Integrity(*integrity)
	if !grade.Valid() {
		pf(stderr, "error: invalid input integrity %q\n", *integrity)
		return 2
	}
	runID, err := telemetry.NewRunID()
	if err != nil {
		pf(stderr, "error: create continuation run id: %v\n", err)
		return 2
	}
	continuation, err := journal.CreateContinuation(
		filepath.Dir(sourceDir),
		journal.ContinuationRequest{
			RunID: runID, SourceRunID: sourceID, ExpectedTerminalSeq: *terminalSeq,
			Operator: *operator, Target: *target, Inputs: inputs,
			InputIntegrity: inputIntegrityMap(inputFlags, grade),
			InputSource:    inputSources,
			SourceBranch:   sourceBranch, ExpectedSourceSHA: sourceSHA,
			SourceRepository: &apiv1.RepoRef{
				Provider: apiv1.Provider(repo.Provider), Owner: repo.Owner,
				Project: repo.Project, Name: repo.Name, BaseURL: repo.URL,
			},
			ContextPointers: contextPointers,
			VerifySourceBranch: func(branch, sha string) error {
				current, found, err := branchProvider.GetBranch(context.Background(), repo, branch)
				if err != nil {
					return err
				}
				if !found {
					return fmt.Errorf("branch does not exist")
				}
				if !strings.EqualFold(current.SHA, sha) {
					return fmt.Errorf("branch head is %q, expected %q", current.SHA, sha)
				}
				return nil
			},
		},
	)
	if err != nil {
		pf(stderr, "error: create continuation: %v\n", err)
		return 1
	}

	if err := continuation.Close(); err != nil {
		pf(stderr, "error: close continuation journal: %v\n", err)
		return 2
	}
	pf(stdout, "%s\n", runID)
	return 0
}

func sameContinuationRepository(source apiv1.RepoRef, configured providers.RepositoryRef) bool {
	return providers.ProviderKind(source.Provider) == configured.Provider &&
		strings.EqualFold(source.BaseURL, configured.URL) &&
		strings.EqualFold(source.Owner, configured.Owner) &&
		strings.EqualFold(source.Project, configured.Project) &&
		strings.EqualFold(source.Name, configured.Name)
}

func continuationBranchNamespace(root, gaggle string) (string, error) {
	set, report, err := instance.LoadConfigDir(instance.NewLayout(root).ConfigDir())
	printValidationIssues(os.Stderr, report)
	if err != nil {
		return "", err
	}
	for _, item := range set.Gaggles {
		if item.Name == gaggle {
			return providers.NormalizeBranchNamespace(item.Spec.BranchNamespace), nil
		}
	}
	return "", fmt.Errorf("gaggle %q is not configured", gaggle)
}

func continuationRepository(root, gaggle string) (providers.RepositoryRef, error) {
	set, report, err := instance.LoadConfigDir(instance.NewLayout(root).ConfigDir())
	printValidationIssues(os.Stderr, report)
	if err != nil {
		return providers.RepositoryRef{}, err
	}
	for _, item := range set.Gaggles {
		if item.Name != gaggle {
			continue
		}
		project := item.Spec.Project
		return providers.RepositoryRef{
			Provider: providers.ProviderKind(project.Provider),
			Owner:    project.Owner, Project: project.Project, Name: project.Name,
			URL: project.BaseURL,
		}, nil
	}
	return providers.RepositoryRef{}, fmt.Errorf("gaggle %q is not configured", gaggle)
}

func selectedContinuationPointers(source []apiv1.ContextPointer, names []string, sourceID string) ([]apiv1.ContextPointer, error) {
	if len(names) == 0 {
		return nil, nil
	}
	byName := make(map[string]apiv1.ContextPointer, len(source))
	for _, pointer := range source {
		byName[pointer.Name] = pointer
	}
	selected := make([]apiv1.ContextPointer, 0, len(names))
	for _, name := range names {
		pointer, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("source artifact pointer %q was not found", name)
		}
		if pointer.Artifact == nil || pointer.External != nil {
			return nil, fmt.Errorf("source pointer %q is not an artifact", name)
		}
		pointer.RunID = sourceID
		selected = append(selected, pointer)
	}
	return selected, nil
}

func currentWorkflowMachine(root string, source journal.RunIdentity) (*workflow.Machine, error) {
	set, report, err := instance.LoadConfigDir(instance.NewLayout(root).ConfigDir())
	printValidationIssues(os.Stderr, report)
	if err != nil {
		return nil, err
	}
	for _, definition := range set.Workflows {
		if definition.Name != source.Workflow || definition.Spec.Gaggle != source.Gaggle {
			continue
		}
		return workflow.Compile(workflow.Definition{
			Name: definition.Name, Version: source.WorkflowVersion,
			DSLVersion: definition.DSLVersion, Spec: definition.Spec,
		}, workflow.WithPreviewFeatures(
			set.Manifest != nil && workflow.PreviewFeaturesEnabled(set.Manifest.Annotations),
		))
	}
	return nil, fmt.Errorf("workflow %q for gaggle %q is not configured", source.Workflow, source.Gaggle)
}

type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }
func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func inputIntegrityMap(inputs stringListFlag, grade apiv1.Integrity) map[string]apiv1.Integrity {
	result := make(map[string]apiv1.Integrity, len(inputs))
	for _, value := range inputs {
		name, _, _ := strings.Cut(value, "=")
		result[name] = grade
	}
	return result
}
