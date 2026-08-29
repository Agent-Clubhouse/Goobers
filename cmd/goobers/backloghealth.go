package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

const backlogHealthHelp = "Usage: goobers backlog-health [--feedback] [path]\n\n" +
	"Snapshot ready-pool depth and age from provider label-event timestamps, and\n" +
	"persist the paginated ready-transition ledger for telemetry rollups.\n" +
	"The ledger resumes from a durable per-repo/label event cursor; a full history\n" +
	"scan runs only on the first cycle or an integrity mismatch, bounded by the\n" +
	"transitionScanMaxPages input. Below the transitionScanQuotaFloor fraction of\n" +
	"the provider rate-limit window the scan defers to the next cycle rather than\n" +
	"spending the shared credential to zero.\n" +
	"--feedback instead de-readies items whose consecutive failed/escalated\n" +
	"implementation runs meet the implementationFailureThreshold input (minimum 2).\n" +
	"Exit codes: 0 = OK, 1 = provider/IO error, 2 = usage error.\n"

const (
	defaultImplementationFailureThreshold = 3
	maxImplementationFailureEvidence      = 5
	// defaultTransitionScanMaxPages bounds a full-history rescan. It is
	// deliberately high — a bound is a safety valve against an unbounded walk,
	// not a tuning knob, and truncating a first-run scan costs a deferral.
	defaultTransitionScanMaxPages = 400
	// defaultTransitionScanQuotaFloor keeps a tenth of the provider rate-limit
	// window in reserve for the operations that actually move work: claims,
	// label writes, PR creation, merge-review. This check is periodic and
	// self-healing, so deferring it is free.
	defaultTransitionScanQuotaFloor = 0.10
)

type backlogHealthReport struct {
	ReadyPoolDepth         int                                 `json:"readyPoolDepth"`
	AverageReadyAgeSeconds float64                             `json:"averageReadyAgeSeconds"`
	OldestReadyAgeSeconds  float64                             `json:"oldestReadyAgeSeconds"`
	ReadyPoolStarved       bool                                `json:"readyPoolStarved"`
	ReadyPoolObservedAt    string                              `json:"readyPoolObservedAt"`
	ReadyTransitions       []providers.WorkItemLabelTransition `json:"readyTransitions,omitempty"`
	Scan                   *backlogHealthScan                  `json:"scan,omitempty"`
}

type implementationFeedbackReport struct {
	ImplementationFailureThreshold int                          `json:"implementationFailureThreshold"`
	Recurated                      int                          `json:"recurated"`
	Items                          []implementationFeedbackItem `json:"items,omitempty"`
	Scan                           *backlogHealthScan           `json:"scan,omitempty"`
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

// labelTransitionScanner is the resumable, self-bounding form of
// repositoryLabelTransitionProvider (#3392). A provider that implements it gets
// the durable-cursor path; one that does not keeps the full-history walk.
type labelTransitionScanner interface {
	ScanWorkItemLabelTransitions(
		context.Context,
		providers.RepositoryRef,
		string,
		providers.LabelTransitionScan,
	) (providers.LabelTransitionScanResult, error)
}

// backlogHealthScanOptions carries the stage-tunable bounds on a transition
// scan, resolved from stage inputs.
type backlogHealthScanOptions struct {
	maxPages   int
	quotaFloor float64
	// forceFull discards any durable cursor and rescans all history. Set only
	// after a resumed ledger failed to explain the live ready pool.
	forceFull bool
}

func resolveBacklogHealthScanOptions(stderr io.Writer) (backlogHealthScanOptions, bool) {
	opts := backlogHealthScanOptions{
		maxPages:   defaultTransitionScanMaxPages,
		quotaFloor: defaultTransitionScanQuotaFloor,
	}
	if raw := providerInput("transitionScanMaxPages", ""); raw != "" {
		pages, err := strconv.Atoi(raw)
		if err != nil || pages < 1 {
			pf(stderr, "error: input transitionScanMaxPages must be an integer of at least 1, got %q\n", raw)
			return opts, false
		}
		opts.maxPages = pages
	}
	if raw := providerInput("transitionScanQuotaFloor", ""); raw != "" {
		floor, err := strconv.ParseFloat(raw, 64)
		if err != nil || floor < 0 || floor >= 1 {
			pf(stderr, "error: input transitionScanQuotaFloor must be a fraction in [0,1), got %q\n", raw)
			return opts, false
		}
		opts.quotaFloor = floor
	}
	return opts, true
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
	scanOpts, ok := resolveBacklogHealthScanOptions(stderr)
	if !ok {
		return 2
	}
	root := providerStageRoot(pathArg)
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	issueProvider, err := newBacklogHealthProvider(root, repo, !*feedback)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	backlogRepo := backlogRepoRefForStage(root, repo)
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
	transitions, scan, err := resolveBacklogReadyTimes(
		ctx, issueProvider, root, backlogRepo, items, readyLabel, scanOpts, stdout)
	if err != nil {
		return backlogReadyLedgerExitCode(err, stderr)
	}
	pf(stdout, "ready-transition scan: mode=%s%s pages=%d new=%d ledger=%d\n",
		scan.Mode, backlogHealthScanReasonSuffix(scan), scan.Pages, scan.NewTransitions, scan.LedgerSize)
	if scan.Deferred {
		pf(stdout, "deferring the ready-pool snapshot to the next cycle: %s\n", scan.DeferReason)
		if *feedback {
			return writeImplementationFeedbackReport(
				implementationFeedbackReport{Scan: &scan}, stdout, stderr)
		}
		return writeBacklogHealthReport(
			backlogHealthReport{Scan: &scan}, stdout, stderr)
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
			scan,
			stdout,
			stderr,
		)
	}

	observedAt := time.Now().UTC()
	ledger, err := openStageClaimLedger(layoutFor(root))
	if err != nil {
		pf(stderr, "error: inspect ready-pool claims: %v\n", err)
		return 1
	}
	claims, err := ledger.ListNamespace(ctx, providerGaggle(), string(backlogRepo.Provider))
	if err != nil {
		pf(stderr, "error: inspect ready-pool claims: %v\n", err)
		return 1
	}
	items = unclaimedReadyItems(items, claims, providerGaggle(), string(backlogRepo.Provider), observedAt)
	report := measureReadyPool(items, readyLabel, observedAt)
	report.ReadyTransitions = transitions
	report.Scan = &scan
	return writeBacklogHealthReport(report, stdout, stderr)
}

func writeBacklogHealthReport(report backlogHealthReport, stdout, stderr io.Writer) int {
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
	if report.ReadyPoolObservedAt == "" {
		// A deferred snapshot deliberately carries no observation: an absent
		// readyPoolObservedAt is what keeps the telemetry rollup from recording
		// "depth 0" — a starvation signal — for a cycle that never measured.
		return 0
	}
	pf(stdout, "ready pool: %d items, oldest age %.0fs\n", report.ReadyPoolDepth, report.OldestReadyAgeSeconds)
	return 0
}

func backlogHealthScanReasonSuffix(scan backlogHealthScan) string {
	if scan.Reason == "" {
		return ""
	}
	return " reason=" + scan.Reason
}

func newBacklogHealthProvider(root string, repo providers.RepositoryRef, readOnly bool) (backlogHealthProvider, error) {
	provider, err := newProviderForStage(root, repo, readOnly, withStageProviderCache(), withStageProviderMutations("issue"))
	if err != nil {
		return nil, err
	}
	healthProvider, ok := provider.(backlogHealthProvider)
	if !ok {
		return nil, fmt.Errorf("backlog-health does not support repository provider %q", repo.Provider)
	}
	return healthProvider, nil
}

func backlogHealthTransitions(
	ctx context.Context,
	provider backlogHealthProvider,
	root string,
	repo providers.RepositoryRef,
	items []providers.WorkItem,
	label string,
	opts backlogHealthScanOptions,
) ([]providers.WorkItemLabelTransition, backlogHealthScan, error) {
	if scanner, ok := provider.(labelTransitionScanner); ok {
		return resumedBacklogHealthTransitions(ctx, scanner, root, repo, label, opts)
	}
	if repositoryProvider, ok := provider.(repositoryLabelTransitionProvider); ok {
		transitions, err := repositoryProvider.ListWorkItemLabelTransitions(ctx, repo, label)
		return transitions, backlogHealthScan{
			Mode:           backlogHealthScanFull,
			Reason:         backlogHealthScanUnsupported,
			NewTransitions: len(transitions),
			LedgerSize:     len(transitions),
		}, err
	}
	if repo.Provider == providers.ProviderADO {
		return nil, backlogHealthScan{Mode: backlogHealthScanFull, Reason: backlogHealthScanUnsupported}, nil
	}
	itemProvider, ok := provider.(itemLabelTransitionProvider)
	if !ok {
		return nil, backlogHealthScan{}, fmt.Errorf("%s does not support work-item label transitions", repo.Provider)
	}
	var transitions []providers.WorkItemLabelTransition
	for _, item := range items {
		itemTransitions, err := itemProvider.ListWorkItemLabelTransitionsForItem(ctx, repo, item.ID, label)
		if err != nil {
			return nil, backlogHealthScan{}, err
		}
		transitions = append(transitions, itemTransitions...)
	}
	return transitions, backlogHealthScan{
		Mode:           backlogHealthScanFull,
		Reason:         backlogHealthScanUnsupported,
		NewTransitions: len(transitions),
		LedgerSize:     len(transitions),
	}, nil
}

// resumedBacklogHealthTransitions reads only the provider events newer than the
// durable high-water mark and folds them into the persisted ledger, falling back
// to a bounded full scan when no usable cursor exists. A truncated scan is
// reported as deferred and leaves the cursor untouched: a partial walk has a gap
// below its oldest transition, so advancing the mark from it would silently lose
// every transition in that gap forever.
func resumedBacklogHealthTransitions(
	ctx context.Context,
	scanner labelTransitionScanner,
	root string,
	repo providers.RepositoryRef,
	label string,
	opts backlogHealthScanOptions,
) ([]providers.WorkItemLabelTransition, backlogHealthScan, error) {
	gaggle := providerGaggle()
	path := layoutFor(root).BacklogHealthCursorPath(
		gaggle, string(repo.Provider), backlogHealthCursorKey(repo), label)

	var (
		cursor backlogHealthCursor
		reason string
	)
	if opts.forceFull {
		reason = backlogHealthScanLedgerMismatch
		if err := discardBacklogHealthCursor(path); err != nil {
			return nil, backlogHealthScan{}, &backlogReadyLedgerError{
				what: "reset ready-transition ledger", err: err,
			}
		}
	} else {
		cursor, reason = readBacklogHealthCursor(path, gaggle, repo, label)
	}

	request := providers.LabelTransitionScan{MinQuotaFraction: opts.quotaFloor}
	report := backlogHealthScan{Mode: backlogHealthScanFull, Reason: reason}
	if reason == "" {
		report.Mode, report.FromEventID = backlogHealthScanIncremental, cursor.HighWaterEventID
		request.AfterEventID = cursor.HighWaterEventID
	} else {
		cursor = backlogHealthCursor{}
		request.MaxPages = opts.maxPages
	}

	result, err := scanner.ScanWorkItemLabelTransitions(ctx, repo, label, request)
	if err != nil {
		return nil, report, err
	}
	report.Pages = result.Pages
	report.NewTransitions = len(result.Transitions)
	report.QuotaLimit, report.QuotaRemaining = result.QuotaLimit, result.QuotaRemaining
	if result.Truncated {
		report.Deferred, report.DeferReason = true, result.StopReason
		return nil, report, nil
	}

	merged := mergeLabelTransitions(cursor.Transitions, result.Transitions)
	highWater := cursor.HighWaterEventID
	if result.HighEventID > highWater {
		highWater = result.HighEventID
	}
	report.LedgerSize, report.ToEventID = len(merged), highWater
	if highWater <= 0 {
		// A repository with no issue events at all: nothing to resume from, so
		// leave the cursor absent rather than persisting an unusable one.
		return merged, report, nil
	}
	if err := writeBacklogHealthCursor(path, backlogHealthCursor{
		Schema:           backlogHealthCursorSchema,
		Gaggle:           gaggle,
		Provider:         string(repo.Provider),
		Repository:       backlogHealthCursorKey(repo),
		Label:            label,
		HighWaterEventID: highWater,
		ScannedAt:        time.Now().UTC(),
		Transitions:      merged,
	}); err != nil {
		return nil, report, &backlogReadyLedgerError{
			what: "persist ready-transition ledger " + path, err: err,
		}
	}
	return merged, report, nil
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

// backlogReadyLedgerError tags which of the stage's two failure shapes a
// ledger resolution hit, so each keeps the exit semantics it had before the
// resume path existed: a provider read failure still writes the typed
// error-code result file executor/shell.go lifts into ErrorInfo, while a local
// consistency or IO failure stays a plain business error.
type backlogReadyLedgerError struct {
	what     string
	provider bool
	err      error
}

func (e *backlogReadyLedgerError) Error() string { return e.what + ": " + e.err.Error() }
func (e *backlogReadyLedgerError) Unwrap() error { return e.err }

// classifyBacklogReadyLedgerError treats an unclassified failure as a provider
// read — the only unclassified thing a ledger resolution does is call the
// provider. Locally-raised failures (cursor IO, ledger consistency) carry their
// own classification already.
func classifyBacklogReadyLedgerError(err error) error {
	var ledgerErr *backlogReadyLedgerError
	if errors.As(err, &ledgerErr) {
		return err
	}
	return &backlogReadyLedgerError{what: "read ready-label transitions", provider: true, err: err}
}

func backlogReadyLedgerExitCode(err error, stderr io.Writer) int {
	var ledgerErr *backlogReadyLedgerError
	if errors.As(err, &ledgerErr) && ledgerErr.provider {
		return failProviderStage(stderr, ledgerErr.what, ledgerErr.err, "backlog-health.json")
	}
	pf(stderr, "error: %v\n", err)
	return 1
}

// resolveBacklogReadyTimes resolves every ready item's ReadyAt from the
// transition ledger, annotating items in place, and returns the ledger slice the
// artifact carries.
//
// A resumed ledger is the one input that can be stale in a way the stage can
// detect: if it cannot explain an item that currently carries the ready label,
// the ledger — not the repository — is wrong. That is the integrity mismatch
// the bounded full rescan exists for, so it is retried once from scratch rather
// than failing the stage.
func resolveBacklogReadyTimes(
	ctx context.Context,
	issueProvider backlogHealthProvider,
	root string,
	repo providers.RepositoryRef,
	items []providers.WorkItem,
	readyLabel string,
	opts backlogHealthScanOptions,
	stdout io.Writer,
) ([]providers.WorkItemLabelTransition, backlogHealthScan, error) {
	transitions, scan, err := backlogHealthTransitions(ctx, issueProvider, root, repo, items, readyLabel, opts)
	if err != nil {
		return nil, scan, classifyBacklogReadyLedgerError(err)
	}
	if scan.Deferred {
		return nil, scan, nil
	}
	filtered := transitionsForItems(transitions, items)
	annotateErr := annotateBacklogReadyTimes(repo.Provider, items, readyLabel, filtered)
	if annotateErr == nil {
		return filtered, scan, nil
	}
	if !scan.resumable() {
		return nil, scan, &backlogReadyLedgerError{what: "snapshot ready backlog", err: annotateErr}
	}
	pf(stdout, "resumed ready-transition ledger does not explain the live ready pool (%v); rescanning full history\n",
		annotateErr)

	opts.forceFull = true
	transitions, scan, err = backlogHealthTransitions(ctx, issueProvider, root, repo, items, readyLabel, opts)
	if err != nil {
		return nil, scan, classifyBacklogReadyLedgerError(err)
	}
	if scan.Deferred {
		return nil, scan, nil
	}
	filtered = transitionsForItems(transitions, items)
	if err := annotateBacklogReadyTimes(repo.Provider, items, readyLabel, filtered); err != nil {
		return nil, scan, &backlogReadyLedgerError{what: "snapshot ready backlog", err: err}
	}
	return filtered, scan, nil
}

func applyImplementationFeedback(
	ctx context.Context,
	root string,
	repo providers.RepositoryRef,
	issueProvider backlogHealthProvider,
	items []providers.WorkItem,
	trustLabel string,
	readyLabel string,
	scan backlogHealthScan,
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
	report := implementationFeedbackReport{ImplementationFailureThreshold: threshold, Scan: &scan}

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
	current, eligible, err := resolveImplementationFeedbackReadyAt(
		ctx, issueProvider, repo, current, readyLabel,
	)
	if err != nil {
		return nil, false, err
	}
	if !eligible {
		return nil, false, nil
	}
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

func resolveImplementationFeedbackReadyAt(
	ctx context.Context,
	issueProvider backlogHealthProvider,
	repo providers.RepositoryRef,
	current providers.WorkItem,
	readyLabel string,
) (providers.WorkItem, bool, error) {
	readTransitions := func() ([]providers.WorkItemLabelTransition, error) {
		return backlogHealthItemTransitions(ctx, issueProvider, repo, current, readyLabel)
	}

	transitions, err := readTransitions()
	if err != nil {
		return providers.WorkItem{}, false, fmt.Errorf("re-read ready-label transitions: %w", err)
	}
	live := []providers.WorkItem{current}
	if err := annotateBacklogReadyTimes(repo.Provider, live, readyLabel, transitions); err == nil {
		return live[0], true, nil
	}

	transitions, err = readTransitions()
	if err != nil {
		return providers.WorkItem{}, false, fmt.Errorf("re-read ready-label transitions: %w", err)
	}
	live = []providers.WorkItem{current}
	if err := annotateBacklogReadyTimes(repo.Provider, live, readyLabel, transitions); err != nil {
		return providers.WorkItem{}, false, nil
	}
	return live[0], true, nil
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

func unclaimedReadyItems(
	items []providers.WorkItem,
	claims claimsclient.Listing,
	gaggle, provider string,
	observedAt time.Time,
) []providers.WorkItem {
	available := items[:0]
	for _, item := range items {
		key := claimsclient.Key{ExternalID: item.ID}
		if gaggle != "" {
			key = claimsclient.Key{Gaggle: gaggle, Provider: provider, ExternalID: item.ID}
		}
		entry, ok := claims.Lookup(key)
		if ok && entry.ExpiresAt.After(observedAt) {
			continue
		}
		available = append(available, item)
	}
	return available
}
