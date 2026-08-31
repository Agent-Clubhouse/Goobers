package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

const backlogHealthHelp = "Usage: goobers backlog-health [--feedback] [path]\n\n" +
	"Snapshot ready-pool depth and age from provider label-event timestamps, and\n" +
	"persist the paginated ready-transition ledger for telemetry rollups.\n" +
	"--feedback instead de-readies items whose consecutive failed/escalated\n" +
	"implementation runs meet the implementationFailureThreshold input (minimum 2).\n" +
	"Exit codes: 0 = OK, 1 = provider/IO error, 2 = usage error.\n"

const (
	defaultImplementationFailureThreshold = 3
	maxImplementationFailureEvidence      = 5
)

type backlogHealthReport struct {
	ReadyPoolDepth         int                                 `json:"readyPoolDepth"`
	AverageReadyAgeSeconds float64                             `json:"averageReadyAgeSeconds"`
	OldestReadyAgeSeconds  float64                             `json:"oldestReadyAgeSeconds"`
	ReadyPoolStarved       bool                                `json:"readyPoolStarved"`
	ReadyPoolObservedAt    string                              `json:"readyPoolObservedAt"`
	ReadyTransitions       []providers.WorkItemLabelTransition `json:"readyTransitions,omitempty"`
}

type implementationFeedbackReport struct {
	ImplementationFailureThreshold int                          `json:"implementationFailureThreshold"`
	Recurated                      int                          `json:"recurated"`
	Items                          []implementationFeedbackItem `json:"items,omitempty"`
}

type implementationFeedbackItem struct {
	ItemID              string                         `json:"itemId"`
	ReadyAt             time.Time                      `json:"readyAt"`
	ConsecutiveFailures int                            `json:"consecutiveFailures"`
	Evidence            []rollup.ImplementationOutcome `json:"evidence"`
}

type backlogHealthProvider interface {
	providers.BacklogProvider
}

type itemLabelTransitionProvider interface {
	ListWorkItemLabelTransitionsForItem(context.Context, providers.RepositoryRef, string, string) ([]providers.WorkItemLabelTransition, error)
}

type repositoryLabelTransitionProvider interface {
	ListWorkItemLabelTransitions(context.Context, providers.RepositoryRef, string) ([]providers.WorkItemLabelTransition, error)
}

func runBacklogHealth(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("backlog-health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	feedback := fs.Bool("feedback", false, "route chronically failing ready items back to curation")
	fs.Usage = helpUsage(stderr, "backlog-health")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	backlogRepo := backlogRepoRefForStage(root, repo)
	issueProvider, err := newBacklogHealthProvider(root, repo, backlogRepo, !*feedback)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	trustLabel := providerInput("trustLabel", "")
	readyLabel := providerInput("readyLabel", providers.LabelReady)
	var labels []string
	if trustLabel != "" {
		labels = []string{trustLabel}
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	if *feedback {
		if err := invalidateCurrentProviderSnapshot(root); err != nil {
			pf(stderr, "error: invalidate provider snapshot before implementation feedback: %v\n", err)
			return 1
		}
	}
	items, err := issueProvider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: backlogRepo,
		Labels:     labels,
		State:      "all",
	})
	if err != nil {
		return failProviderStage(stderr, "snapshot ready backlog", err, "backlog-health.json")
	}
	transitions, err := backlogHealthTransitions(ctx, issueProvider, backlogRepo, items, readyLabel)
	if err != nil {
		return failProviderStage(stderr, "read ready-label transitions", err, "backlog-health.json")
	}
	transitions = transitionsForItems(transitions, items)
	if err := annotateBacklogReadyTimes(backlogRepo.Provider, items, readyLabel, transitions); err != nil {
		pf(stderr, "error: snapshot ready backlog: %v\n", err)
		return 1
	}
	if *feedback {
		return applyImplementationFeedback(
			ctx,
			root,
			backlogRepo,
			issueProvider,
			items,
			trustLabel,
			readyLabel,
			stdout,
			stderr,
		)
	}

	observedAt := time.Now().UTC()
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		pf(stderr, "error: inspect ready-pool claims: %v\n", err)
		return 1
	}
	backlogIdentity := apiv1.BacklogIdentity{}
	if providerGaggle() != "" {
		if identity, err := backlogIdentityForStage(root, repo); err == nil {
			backlogIdentity = identity
		}
	}
	items = unclaimedReadyItems(items, ledger, backlogIdentity, providerGaggle(), string(backlogRepo.Provider), observedAt)
	report := measureReadyPool(items, readyLabel, observedAt)
	report.ReadyTransitions = transitions
	data, err := json.Marshal(report)
	if err != nil {
		pf(stderr, "error: marshal backlog health: %v\n", err)
		return 1
	}
	resultFile := providerInput("resultFile", "backlog-health.json")
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "ready pool: %d items, oldest age %.0fs\n", report.ReadyPoolDepth, report.OldestReadyAgeSeconds)
	return 0
}

// newBacklogHealthProvider builds the health/feedback provider. Every call it
// makes is addressed at the backlog, so it authenticates as the backlog's
// connection rather than the project credential.
func newBacklogHealthProvider(root string, repo, backlogRepo providers.RepositoryRef, readOnly bool) (backlogHealthProvider, error) {
	provider, err := newBacklogProviderForStage(root, repo, backlogRepo, readOnly, withStageProviderCache(), withStageProviderMutations("issue"))
	if err != nil {
		return nil, err
	}
	healthProvider, ok := provider.(backlogHealthProvider)
	if !ok {
		return nil, fmt.Errorf("backlog-health does not support repository provider %q", backlogRepo.Provider)
	}
	return healthProvider, nil
}

func backlogHealthTransitions(
	ctx context.Context,
	provider backlogHealthProvider,
	repo providers.RepositoryRef,
	items []providers.WorkItem,
	label string,
) ([]providers.WorkItemLabelTransition, error) {
	if repositoryProvider, ok := provider.(repositoryLabelTransitionProvider); ok {
		return repositoryProvider.ListWorkItemLabelTransitions(ctx, repo, label)
	}
	if repo.Provider == providers.ProviderADO {
		return nil, nil
	}
	itemProvider, ok := provider.(itemLabelTransitionProvider)
	if !ok {
		return nil, fmt.Errorf("%s does not support work-item label transitions", repo.Provider)
	}
	var transitions []providers.WorkItemLabelTransition
	for _, item := range items {
		itemTransitions, err := itemProvider.ListWorkItemLabelTransitionsForItem(ctx, repo, item.ID, label)
		if err != nil {
			return nil, err
		}
		transitions = append(transitions, itemTransitions...)
	}
	return transitions, nil
}

func backlogHealthItemTransitions(
	ctx context.Context,
	provider backlogHealthProvider,
	repo providers.RepositoryRef,
	item providers.WorkItem,
	label string,
) ([]providers.WorkItemLabelTransition, error) {
	if repo.Provider == providers.ProviderADO {
		return nil, nil
	}
	itemProvider, ok := provider.(itemLabelTransitionProvider)
	if !ok {
		return nil, fmt.Errorf("%s does not support work-item label transitions", repo.Provider)
	}
	return itemProvider.ListWorkItemLabelTransitionsForItem(ctx, repo, item.ID, label)
}

func applyImplementationFeedback(
	ctx context.Context,
	root string,
	repo providers.RepositoryRef,
	issueProvider backlogHealthProvider,
	items []providers.WorkItem,
	trustLabel string,
	readyLabel string,
	stdout, stderr io.Writer,
) int {
	threshold, err := strconv.Atoi(providerInput(
		"implementationFailureThreshold",
		strconv.Itoa(defaultImplementationFailureThreshold),
	))
	if err != nil || threshold < 2 {
		pf(stderr, "error: implementationFailureThreshold must be an integer of at least 2\n")
		return 1
	}
	report := implementationFeedbackReport{ImplementationFailureThreshold: threshold}

	var earliestReadyAt time.Time
	for _, item := range items {
		if !implementationFeedbackEligible(item, readyLabel) {
			continue
		}
		if earliestReadyAt.IsZero() || item.ReadyAt.Before(earliestReadyAt) {
			earliestReadyAt = *item.ReadyAt
		}
	}
	if earliestReadyAt.IsZero() {
		return writeImplementationFeedbackReport(report, stdout, stderr)
	}

	dbPath := layoutFor(root).TelemetryDB()
	info, err := os.Stat(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return writeImplementationFeedbackReport(report, stdout, stderr)
		}
		pf(stderr, "error: inspect telemetry rollup %s: %v\n", dbPath, err)
		return 1
	}
	if info.Size() == 0 {
		return writeImplementationFeedbackReport(report, stdout, stderr)
	}
	db, err := rollup.Open(dbPath)
	if err != nil {
		pf(stderr, "error: open telemetry rollup %s: %v\n", dbPath, err)
		return 1
	}
	defer func() { _ = db.Close() }()

	outcomes, err := db.ImplementationOutcomes(ctx, providerGaggle(), earliestReadyAt)
	if err != nil {
		pf(stderr, "error: query implementation outcomes: %v\n", err)
		return 1
	}
	mutationAttempted := false
	for _, item := range items {
		if !implementationFeedbackEligible(item, readyLabel) {
			continue
		}
		count, _ := consecutiveImplementationFailures(outcomes, item.ID, *item.ReadyAt)
		if count < threshold {
			continue
		}
		recurated, attempted, err := reCurateImplementationFeedbackItem(
			ctx,
			layoutFor(root),
			repo,
			issueProvider,
			outcomes,
			item.ID,
			trustLabel,
			readyLabel,
			threshold,
		)
		mutationAttempted = mutationAttempted || attempted
		if err != nil {
			if mutationAttempted {
				err = errors.Join(err, invalidateCurrentProviderSnapshot(root))
			}
			pf(stderr, "error: route issue %s back to curation: %v\n", item.ID, err)
			return 1
		}
		if recurated == nil {
			continue
		}
		report.Recurated++
		report.Items = append(report.Items, *recurated)
	}
	if mutationAttempted {
		if err := invalidateCurrentProviderSnapshot(root); err != nil {
			pf(stderr, "error: invalidate provider snapshot after implementation feedback: %v\n", err)
			return 1
		}
	}
	return writeImplementationFeedbackReport(report, stdout, stderr)
}

func reCurateImplementationFeedbackItem(
	ctx context.Context,
	layout instance.Layout,
	repo providers.RepositoryRef,
	issueProvider backlogHealthProvider,
	outcomes []rollup.ImplementationOutcome,
	itemID, trustLabel, readyLabel string,
	threshold int,
) (result *implementationFeedbackItem, mutationAttempted bool, err error) {
	reservation, acquired, err := reserveBacklogClaimReconciliation(layout, repo, itemID, time.Now)
	if err != nil {
		return nil, false, fmt.Errorf("reserve item against implementation claims: %w", err)
	}
	if !acquired {
		return nil, false, nil
	}
	defer func() {
		if releaseErr := releaseBacklogClaimReconciliation(layout, *reservation); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release implementation-feedback reservation: %w", releaseErr))
		}
	}()

	current, err := issueProvider.GetWorkItem(ctx, repo, itemID)
	if err != nil {
		return nil, false, fmt.Errorf("re-read issue: %w", err)
	}
	if !implementationFeedbackEligibleWithoutReadyAt(current, trustLabel, readyLabel) {
		return nil, false, nil
	}
	transitions, err := backlogHealthItemTransitions(ctx, issueProvider, repo, current, readyLabel)
	if err != nil {
		return nil, false, fmt.Errorf("re-read ready-label transitions: %w", err)
	}
	live := []providers.WorkItem{current}
	if err := annotateBacklogReadyTimes(repo.Provider, live, readyLabel, transitions); err != nil {
		return nil, false, fmt.Errorf("resolve current ready cohort: %w", err)
	}
	current = live[0]
	count, evidence := consecutiveImplementationFailures(outcomes, itemID, *current.ReadyAt)
	if count < threshold {
		return nil, false, nil
	}

	marker := implementationFeedbackMarker(*current.ReadyAt)
	comments, err := issueProvider.ListComments(ctx, repo, itemID)
	if err != nil {
		return nil, false, fmt.Errorf("read prior feedback comments: %w", err)
	}
	comment := implementationFeedbackComment(count, evidence) + "\n\n" + marker
	for _, existing := range comments {
		if strings.Contains(existing.Body, marker) {
			comment = ""
			break
		}
	}

	mutationAttempted = true
	if _, err := issueProvider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository:   repo,
		ID:           itemID,
		Comment:      comment,
		RemoveLabels: []string{readyLabel},
	}); err != nil {
		return nil, true, err
	}
	return &implementationFeedbackItem{
		ItemID:              itemID,
		ReadyAt:             *current.ReadyAt,
		ConsecutiveFailures: count,
		Evidence:            evidence,
	}, true, nil
}

func implementationFeedbackEligibleWithoutReadyAt(
	item providers.WorkItem,
	trustLabel, readyLabel string,
) bool {
	return item.HasLabel(readyLabel) &&
		(trustLabel == "" || item.HasLabel(trustLabel)) &&
		(item.State == "" || strings.EqualFold(item.State, "open"))
}

func implementationFeedbackEligible(item providers.WorkItem, readyLabel string) bool {
	return implementationFeedbackEligibleWithoutReadyAt(item, "", readyLabel) &&
		item.ReadyAt != nil
}

func consecutiveImplementationFailures(
	outcomes []rollup.ImplementationOutcome,
	itemID string,
	readyAt time.Time,
) (int, []rollup.ImplementationOutcome) {
	var failures []rollup.ImplementationOutcome
	for _, outcome := range outcomes {
		if outcome.ItemID != itemID || outcome.StartedAt.Before(readyAt) {
			continue
		}
		switch outcome.Status {
		case "failed", "escalated":
			failures = append(failures, outcome)
		default:
			failures = nil
		}
	}
	count := len(failures)
	if len(failures) > maxImplementationFailureEvidence {
		failures = failures[len(failures)-maxImplementationFailureEvidence:]
	}
	return count, failures
}

func implementationFeedbackComment(count int, evidence []rollup.ImplementationOutcome) string {
	var comment strings.Builder
	fmt.Fprintf(
		&comment,
		"Implementation re-curation requested after %d consecutive failed/escalated run(s) since this item was readied. "+
			"`goobers:ready` was removed so the curator can re-scope it before another implementation attempt.\n\nEvidence:\n",
		count,
	)
	for _, outcome := range evidence {
		at := outcome.FinishedAt
		if at.IsZero() {
			at = outcome.StartedAt
		}
		fmt.Fprintf(&comment, "- run `%s` - %s at %s", outcome.RunID, outcome.Status, at.UTC().Format(time.RFC3339))
		switch {
		case outcome.ErrorCode != "":
			fmt.Fprintf(&comment, "; stage `%s`, `%s`", outcome.Stage, outcome.ErrorCode)
			if message := compactFeedbackText(outcome.ErrorMessage, 240); message != "" {
				fmt.Fprintf(&comment, ": %s", message)
			}
		case outcome.Gate != "":
			fmt.Fprintf(&comment, "; gate `%s` returned `%s`", outcome.Gate, outcome.Verdict)
		}
		comment.WriteByte('\n')
	}
	comment.WriteString(
		"\nThe curator should verify currentness and scope, then either re-ready the item with a revised plan or add " +
			"`goobers:needs-human` with an actionable `For the human` block.",
	)
	return comment.String()
}

func implementationFeedbackMarker(readyAt time.Time) string {
	return fmt.Sprintf(
		"<!-- goobers:implementation-feedback ready-at=%s -->",
		readyAt.UTC().Format(time.RFC3339Nano),
	)
}

func compactFeedbackText(raw string, limit int) string {
	text := strings.Join(strings.Fields(raw), " ")
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-3]) + "..."
}

func writeImplementationFeedbackReport(report implementationFeedbackReport, stdout, stderr io.Writer) int {
	data, err := json.Marshal(report)
	if err != nil {
		pf(stderr, "error: marshal implementation feedback: %v\n", err)
		return 1
	}
	resultFile := providerInput("resultFile", "implementation-feedback.json")
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "routed %d chronically failing item(s) back to curation\n", report.Recurated)
	return 0
}

func measureReadyPool(items []providers.WorkItem, readyLabel string, observedAt time.Time) backlogHealthReport {
	report := backlogHealthReport{ReadyPoolObservedAt: observedAt.UTC().Format(time.RFC3339Nano)}
	var totalAge float64
	for _, item := range items {
		if !item.HasLabel(readyLabel) || (item.State != "" && !strings.EqualFold(item.State, "open")) {
			continue
		}
		age := float64(0)
		if item.ReadyAt != nil && observedAt.After(*item.ReadyAt) {
			age = observedAt.Sub(*item.ReadyAt).Seconds()
		}
		report.ReadyPoolDepth++
		totalAge += age
		if age > report.OldestReadyAgeSeconds {
			report.OldestReadyAgeSeconds = age
		}
	}
	report.ReadyPoolStarved = report.ReadyPoolDepth == 0
	if report.ReadyPoolDepth > 0 {
		report.AverageReadyAgeSeconds = totalAge / float64(report.ReadyPoolDepth)
	}
	return report
}

func transitionsForItems(
	transitions []providers.WorkItemLabelTransition,
	items []providers.WorkItem,
) []providers.WorkItemLabelTransition {
	ids := make(map[string]bool, len(items))
	for _, item := range items {
		ids[item.ID] = true
	}
	filtered := make([]providers.WorkItemLabelTransition, 0, len(transitions))
	for _, transition := range transitions {
		if ids[transition.ItemID] {
			filtered = append(filtered, transition)
		}
	}
	return filtered
}

func annotateReadyTimes(
	items []providers.WorkItem,
	readyLabel string,
	transitions []providers.WorkItemLabelTransition,
) error {
	ordered := append([]providers.WorkItemLabelTransition(nil), transitions...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].OccurredAt.Equal(ordered[j].OccurredAt) {
			return ordered[i].EventID < ordered[j].EventID
		}
		return ordered[i].OccurredAt.Before(ordered[j].OccurredAt)
	})
	active := make(map[string]time.Time)
	for _, transition := range ordered {
		if transition.Label != readyLabel {
			continue
		}
		if transition.Added {
			active[transition.ItemID] = transition.OccurredAt
		} else {
			delete(active, transition.ItemID)
		}
	}
	for i := range items {
		if !items[i].HasLabel(readyLabel) ||
			(items[i].State != "" && !strings.EqualFold(items[i].State, "open")) {
			continue
		}
		readyAt, ok := active[items[i].ID]
		if !ok {
			return fmt.Errorf("issue %s has %q but no active label-add event", items[i].ID, readyLabel)
		}
		items[i].ReadyAt = &readyAt
	}
	return nil
}

func annotateBacklogReadyTimes(
	provider providers.ProviderKind,
	items []providers.WorkItem,
	readyLabel string,
	transitions []providers.WorkItemLabelTransition,
) error {
	if provider != providers.ProviderADO {
		return annotateReadyTimes(items, readyLabel, transitions)
	}
	for i := range items {
		if !items[i].HasLabel(readyLabel) {
			continue
		}
		// ADO does not expose tag history. ChangedDate is only a conservative
		// timestamp for the current ready cohort, not a provider transition.
		if items[i].UpdatedAt == nil {
			return fmt.Errorf("ADO work item %s has %q but no ChangedDate", items[i].ID, readyLabel)
		}
		items[i].ReadyAt = items[i].UpdatedAt
	}
	return nil
}

// unclaimedReadyItems removes items that currently carry a live lease.
//
// The backlog-scoped lookup is deliberately gaggle-agnostic: ready-pool depth
// is a property of the BACKLOG, so an item leased by a sibling gaggle that
// shares this backlog is genuinely not available work and must not be counted
// as ready-pool depth for this one. backlog is zero only for a standalone
// invocation that could not resolve one, which keeps the historical
// gaggle-scoped behavior.
func unclaimedReadyItems(
	items []providers.WorkItem,
	ledger *localscheduler.ClaimLedger,
	backlog apiv1.BacklogIdentity,
	gaggle, provider string,
	observedAt time.Time,
) []providers.WorkItem {
	if ledger == nil {
		return items
	}
	live := make(map[string]struct{})
	if !backlog.IsZero() {
		for _, entry := range ledger.Snapshot() {
			identity, ok := entry.BacklogIdentity()
			if !ok || !identity.Equal(backlog) || !entry.ExpiresAt.After(observedAt) {
				continue
			}
			live[entry.ExternalID] = struct{}{}
		}
	}
	available := items[:0]
	for _, item := range items {
		if _, held := live[item.ID]; held {
			continue
		}
		var (
			entry localscheduler.ClaimEntry
			ok    bool
		)
		if gaggle == "" {
			entry, ok = ledger.Lookup(item.ID)
		} else {
			entry, ok = ledger.LookupScoped(localscheduler.ClaimKey{
				Gaggle: gaggle, Provider: provider, ExternalID: item.ID,
			})
		}
		if ok && entry.ExpiresAt.After(observedAt) {
			continue
		}
		available = append(available, item)
	}
	return available
}
