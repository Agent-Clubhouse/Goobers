package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/backlogdefaults"
	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/labelpredicate"
	"github.com/goobers/goobers/internal/localscheduler"
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
	sourceBranch, sourceSHA, err := continuationSourceBranch(sourceReader, sourceIdentity, *branch, *expectedSHA)
	if err != nil {
		pf(stderr, "error: read continuation source events: %v\n", err)
		return 1
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
	claims, err := claimHistoryForRun(layoutFor(root), sourceID, apiv1.Provider(repo.Provider))
	if err != nil {
		pf(stderr, "error: revalidate continuation claims: %v\n", err)
		return 1
	}
	if err := validateContinuationClaims(root, claims, sourceIdentity, provider, repo); err != nil {
		pf(stderr, "error: %v\n", err)
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
	request := journal.ContinuationRequest{
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
	}
	runID, err = createOrReuseContinuation(root, filepath.Dir(sourceDir), sourceIdentity, claims, request)
	if err != nil {
		pf(stderr, "error: create continuation: %v\n", err)
		return 1
	}
	pf(stdout, "%s\n", runID)
	return 0
}

func continuationSourceBranch(sourceReader *journal.Reader, sourceIdentity journal.RunIdentity, requestedBranch, expectedSHA string) (string, string, error) {
	sourceBranch := strings.TrimSpace(requestedBranch)
	if sourceBranch == "" {
		sourceBranch = strings.TrimSpace(sourceIdentity.WorkspaceBranch)
	}
	sourceSHA := strings.TrimSpace(expectedSHA)
	if sourceSHA == "" {
		sourceSHA = strings.TrimSpace(sourceIdentity.WorkspaceBranchSHA)
	}
	sourceEvents, err := sourceReader.Events()
	if err != nil {
		return "", "", err
	}
	var recordedBranch, recordedSHA string
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
	return sourceBranch, sourceSHA, nil
}

type continuationEligibilityProvider interface {
	GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error)
}

type continuationPullRequestProvider interface {
	PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error)
}

type continuationEligibilityPolicy struct {
	requireLabels   []string
	excludeLabels   []string
	labelFilter     *labelpredicate.Predicate
	fieldFilter     *fieldpredicate.Predicate
	respectAssignee bool
	assignedTo      string
}

func validateContinuationClaims(root string, claims []localscheduler.ClaimEntry, sourceIdentity journal.RunIdentity, provider providers.Provider, repo providers.RepositoryRef) error {
	if len(claims) == 0 {
		return nil
	}
	workItemProvider, ok := provider.(continuationEligibilityProvider)
	if !ok {
		return fmt.Errorf("revalidate continuation claims: provider %q cannot read claimed work items", repo.Provider)
	}
	pullRequestProvider, _ := provider.(continuationPullRequestProvider)
	workItemRepo, err := continuationWorkItemRepository(root, sourceIdentity.Gaggle, repo)
	if err != nil {
		return fmt.Errorf("revalidate continuation claims: %w", err)
	}
	policy, err := continuationEligibilityPolicyFor(root, sourceIdentity)
	if err != nil {
		return fmt.Errorf("revalidate continuation claims: %w", err)
	}
	ctx, cancel := providerCommandContext()
	defer cancel()
	for _, claim := range claims {
		claimID := continuationClaimID(claim)
		if claim.Provider != "" && !strings.EqualFold(claim.Provider, string(repo.Provider)) {
			return fmt.Errorf("revalidate continuation claims: source claim %q uses provider %q, configured repository uses %q", claimID, claim.Provider, repo.Provider)
		}
		if strings.HasPrefix(claimID, pullRequestClaimPrefix) {
			if pullRequestProvider == nil {
				return fmt.Errorf("revalidate continuation claims: provider %q cannot read claimed pull requests", repo.Provider)
			}
			poll, err := pullRequestProvider.PollPullRequest(ctx, providers.PullRequestPollRequest{
				Repository: repo,
				PullID:     strings.TrimPrefix(claimID, pullRequestClaimPrefix),
			})
			switch {
			case providers.IsNotFoundError(err):
				return fmt.Errorf("revalidate continuation claims: source claim %q no longer resolves in %s", claimID, repositoryDisplayName(repo))
			case err != nil:
				return fmt.Errorf("revalidate continuation claims: read claimed pull request %q: %w", claimID, err)
			case poll.State != "" && !strings.EqualFold(poll.State, "open"):
				return fmt.Errorf("revalidate continuation claims: source claim %q is no longer open (state %q)", claimID, poll.State)
			}
			continue
		}
		item, err := workItemProvider.GetWorkItem(ctx, workItemRepo, claimID)
		switch {
		case providers.IsNotFoundError(err):
			return fmt.Errorf("revalidate continuation claims: source claim %q no longer resolves in %s", claimID, repositoryDisplayName(workItemRepo))
		case err != nil:
			return fmt.Errorf("revalidate continuation claims: read claimed item %q: %w", claimID, err)
		case item.State != "" && !strings.EqualFold(item.State, "open"):
			return fmt.Errorf("revalidate continuation claims: source claim %q is no longer open (state %q)", claimID, item.State)
		}
		if err := validateContinuationEligibility(item, claimID, policy); err != nil {
			return fmt.Errorf("revalidate continuation claims: %w", err)
		}
	}
	return nil
}

func createOrReuseContinuation(root, runsDir string, sourceIdentity journal.RunIdentity, claims []localscheduler.ClaimEntry, req journal.ContinuationRequest) (string, error) {
	lockRunID, err := findMatchingContinuationRunID(runsDir, req)
	if err != nil {
		return "", err
	}
	if lockRunID == "" {
		lockRunID = req.RunID
	}
	layout := layoutFor(root)
	lockPath := filepath.Join(layout.SchedulerDir(), claimLockFileName)
	var continuationRunID string
	var created bool
	if err := withClaimLockForRun(lockPath, claimLockOperationContinuationReacquire, sourceIdentity.Gaggle, lockRunID, func() error {
		continuationRunID, err = findMatchingContinuationRunID(runsDir, req)
		if err != nil {
			return err
		}
		if continuationRunID == "" {
			continuation, err := journal.CreateContinuation(runsDir, req)
			if err != nil {
				return err
			}
			created = true
			continuationRunID = req.RunID
			if err := continuation.Close(); err != nil {
				rollbackErr := deleteContinuationRun(filepath.Join(runsDir, continuationRunID))
				if rollbackErr != nil {
					err = errors.Join(err, rollbackErr)
				}
				return err
			}
		}
		return reclaimContinuationClaimsLocked(layout, claims, sourceIdentity.Workflow, continuationRunID)
	}); err != nil {
		if created {
			rollbackErr := deleteContinuationRun(filepath.Join(runsDir, continuationRunID))
			if rollbackErr != nil {
				err = errors.Join(err, rollbackErr)
			}
		}
		return "", err
	}
	return continuationRunID, nil
}

func reclaimContinuationClaimsLocked(layout instance.Layout, claims []localscheduler.ClaimEntry, workflow, continuationRunID string) error {
	if len(claims) == 0 {
		return nil
	}
	var acquired bool
	var holder string
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName))
	if err != nil {
		return err
	}
	acquired, holder, err = ledger.ReclaimAll(claims, continuationRunID, workflow, DefaultClaimLease)
	if err != nil {
		return err
	}
	if !acquired {
		return fmt.Errorf("source claims are now held by run %q", holder)
	}
	return nil
}

func findMatchingContinuationRunID(runsDir string, req journal.ContinuationRequest) (string, error) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return "", fmt.Errorf("inspect continuation runs: %w", err)
	}
	matches := make([]string, 0, 1)
	for _, entry := range entries {
		if !entry.IsDir() || !apiv1.ValidRunID(entry.Name()) {
			continue
		}
		reader, err := journal.OpenReadOnly(filepath.Join(runsDir, entry.Name()))
		if err != nil {
			return "", fmt.Errorf("inspect continuation run %q: %w", entry.Name(), err)
		}
		identity, err := reader.Identity()
		if err != nil {
			return "", fmt.Errorf("read continuation run %q identity: %w", entry.Name(), err)
		}
		if continuationIdentityMatches(identity, req) {
			matches = append(matches, entry.Name())
		}
	}
	if len(matches) == 0 {
		return "", nil
	}
	sort.Strings(matches)
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple continuations already exist for source run %q target %q terminal seq %d: %s",
			req.SourceRunID, req.Target, req.ExpectedTerminalSeq, strings.Join(matches, ", "))
	}
	return matches[0], nil
}

func continuationIdentityMatches(identity journal.RunIdentity, req journal.ContinuationRequest) bool {
	if identity.ContinuedFromRunID != req.SourceRunID ||
		identity.SourceTerminalSeq != req.ExpectedTerminalSeq ||
		identity.Operator != strings.TrimSpace(req.Operator) ||
		identity.RequestedTarget != strings.TrimSpace(req.Target) ||
		identity.WorkspaceBranch != strings.TrimSpace(req.SourceBranch) ||
		identity.WorkspaceBranchSHA != strings.TrimSpace(req.ExpectedSourceSHA) {
		return false
	}
	if !continuationRepositoriesMatch(identity.WorkspaceRepository, req.SourceRepository) {
		return false
	}
	if !continuationPointersMatch(identity.ContextPointers, normalizedContinuationPointers(req.ContextPointers, req.SourceRunID)) {
		return false
	}
	return continuationInputsMatch(identity.Inputs, req)
}

func continuationRepositoriesMatch(existing, requested *apiv1.RepoRef) bool {
	switch {
	case existing == nil && requested == nil:
		return true
	case existing == nil || requested == nil:
		return false
	default:
		return existing.Provider == requested.Provider &&
			strings.EqualFold(existing.BaseURL, requested.BaseURL) &&
			strings.EqualFold(existing.Owner, requested.Owner) &&
			strings.EqualFold(existing.Project, requested.Project) &&
			strings.EqualFold(existing.Name, requested.Name)
	}
}

func normalizedContinuationPointers(pointers []apiv1.ContextPointer, sourceRunID string) []apiv1.ContextPointer {
	normalized := make([]apiv1.ContextPointer, len(pointers))
	copy(normalized, pointers)
	for i := range normalized {
		if normalized[i].RunID == "" {
			normalized[i].RunID = sourceRunID
		}
	}
	return normalized
}

func continuationPointersMatch(existing, requested []apiv1.ContextPointer) bool {
	if len(existing) == 0 && len(requested) == 0 {
		return true
	}
	return reflect.DeepEqual(existing, requested)
}

func continuationInputsMatch(existing []journal.InputRef, req journal.ContinuationRequest) bool {
	if len(existing) != len(req.Inputs) {
		return false
	}
	for _, input := range existing {
		data, ok := req.Inputs[input.Name]
		if !ok {
			return false
		}
		if input.Ref.Path != filepath.ToSlash(filepath.Join("inputs", input.Name)) ||
			input.Ref.Digest != journal.Digest(data) ||
			input.Source != req.InputSource[input.Name] ||
			input.Integrity != req.InputIntegrity[input.Name] {
			return false
		}
	}
	return true
}

func continuationClaimID(claim localscheduler.ClaimEntry) string {
	if claim.ExternalID != "" {
		return claim.ExternalID
	}
	return claim.ItemID
}

func validateContinuationEligibility(item providers.WorkItem, claimID string, policy *continuationEligibilityPolicy) error {
	if policy == nil {
		return nil
	}
	if policy.respectAssignee && item.Assignee != policy.assignedTo {
		return fmt.Errorf("source claim %q is assigned to %q, need %q", claimID, item.Assignee, policy.assignedTo)
	}
	matched, err := policy.labelFilter.Matches(item.Labels)
	if err != nil {
		return fmt.Errorf("evaluate workflow label eligibility for %q: %w", claimID, err)
	}
	if !matched {
		return fmt.Errorf("source claim %q no longer matches workflow label eligibility (%s)", claimID, continuationLabelExclusionReason(item, policy))
	}
	matched, err = policy.fieldFilter.Matches(item.Fields)
	if err != nil {
		return fmt.Errorf("evaluate workflow field eligibility for %q: %w", claimID, err)
	}
	if !matched {
		return fmt.Errorf("source claim %q no longer matches workflow field eligibility", claimID)
	}
	return nil
}

func continuationLabelExclusionReason(item providers.WorkItem, policy *continuationEligibilityPolicy) string {
	for _, label := range policy.requireLabels {
		if !item.HasLabel(label) {
			return fmt.Sprintf("missing required label %q", label)
		}
	}
	for _, label := range policy.excludeLabels {
		if item.HasLabel(label) {
			return fmt.Sprintf("has excluded label %q", label)
		}
	}
	return "label predicate not matched"
}

func continuationEligibilityPolicyFor(root string, source journal.RunIdentity) (*continuationEligibilityPolicy, error) {
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		return nil, err
	}
	set, report, err := instance.LoadConfigDir(instance.NewLayout(root).ConfigDir())
	printValidationIssues(os.Stderr, report)
	if err != nil {
		return nil, err
	}
	var gaggle *apiv1.Gaggle
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == source.Gaggle {
			gaggle = &set.Gaggles[i]
			break
		}
	}
	if gaggle == nil {
		return nil, fmt.Errorf("gaggle %q is not configured", source.Gaggle)
	}
	var definition *apiv1.Workflow
	for i := range set.Workflows {
		if set.Workflows[i].Name == source.Workflow && set.Workflows[i].Spec.Gaggle == source.Gaggle {
			definition = &set.Workflows[i]
			break
		}
	}
	if definition == nil {
		return nil, fmt.Errorf("workflow %q for gaggle %q is not configured", source.Workflow, source.Gaggle)
	}
	for _, trigger := range definition.Spec.Triggers {
		if trigger.Type != apiv1.TriggerBacklogItem {
			continue
		}
		labels := make([]string, 0, len(trigger.Selector))
		for label := range trigger.Selector {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		labelFilter, err := labelpredicate.Compile(trigger.LabelPredicate, labels, nil)
		if err != nil {
			return nil, fmt.Errorf("workflow %q backlog label predicate: %w", definition.Name, err)
		}
		fieldFilter, err := fieldpredicate.CompileConjunction(gaggle.Spec.Backlog.FieldPredicate, trigger.FieldPredicate)
		if err != nil {
			return nil, fmt.Errorf("workflow %q backlog field predicate: %w", definition.Name, err)
		}
		return &continuationEligibilityPolicy{
			requireLabels: labels,
			labelFilter:   labelFilter,
			fieldFilter:   fieldFilter,
		}, nil
	}
	for i := range definition.Spec.Tasks {
		task := definition.Spec.Tasks[i]
		if task.Name != definition.Spec.Start || !backlogdefaults.IsBacklogQuery(task) {
			continue
		}
		inputs := backlogdefaults.Apply(task, task.Inputs, instance.EffectiveSelfIdentity(cfg, gaggle), strings.Join(gaggle.Spec.RequireLabels, ","))
		requireLabels := splitLabelList(inputs["requireLabels"])
		excludeLabels := splitLabelList(inputs["excludeLabels"])
		labelFilter, excludeLabels, err := compileBacklogLabelSelection(inputs["labelPredicate"], requireLabels, excludeLabels, inputs["parkLabels"], inputs["filterParkLabels"])
		if err != nil {
			return nil, fmt.Errorf("workflow %q refill label predicate: %w", definition.Name, err)
		}
		fieldFilter, err := fieldpredicate.Compile(inputs["fieldPredicate"])
		if err != nil {
			return nil, fmt.Errorf("workflow %q refill field predicate: %w", definition.Name, err)
		}
		return &continuationEligibilityPolicy{
			requireLabels:   requireLabels,
			excludeLabels:   excludeLabels,
			labelFilter:     labelFilter,
			fieldFilter:     fieldFilter,
			respectAssignee: inputs["respectAssignee"] == "true",
			assignedTo:      inputs["assignedTo"],
		}, nil
	}
	return nil, nil
}

func continuationWorkItemRepository(root, gaggle string, routed providers.RepositoryRef) (providers.RepositoryRef, error) {
	if routed.Provider != providers.ProviderADO {
		return routed, nil
	}
	set, report, err := instance.LoadConfigDir(instance.NewLayout(root).ConfigDir())
	if err != nil {
		return providers.RepositoryRef{}, err
	}
	if set == nil || report == nil {
		return routed, nil
	}
	return applyBacklogProject(set, gaggle, routed), nil
}

func deleteContinuationRun(dir string) error {
	err := journal.ClearRunActive(dir)
	if removeErr := os.RemoveAll(dir); removeErr != nil {
		err = errors.Join(err, fmt.Errorf("remove continuation run: %w", removeErr))
	}
	return err
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
		// Preview authorization is per-Workflow (#4220): definition's OWN
		// annotations, never the Manifest's or its gaggle's.
		return workflow.Compile(workflow.Definition{
			Name: definition.Name, Version: source.WorkflowVersion,
			DSLVersion: definition.DSLVersion, Spec: definition.Spec, Annotations: definition.Annotations,
		}, workflow.WithPreviewFeatures(
			workflow.PreviewFeaturesEnabled(definition.Annotations),
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
