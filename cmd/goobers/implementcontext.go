package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/providers"
)

const (
	implementationContextResultFile     = "implementation-context.json"
	defaultImplementationHotFiles       = 100
	maxImplementationHotFiles           = 500
	maxImplementationRefsPerHotFile     = 20
	implementationConflictHistoryWindow = 30 * 24 * time.Hour
)

type verdictFindingClassDigest struct {
	Class   apiv1.FindingClass `json:"class"`
	Meaning string             `json:"meaning"`
}

type reviewerVerdictTaxonomyDigest struct {
	ContractVersion string                      `json:"contractVersion"`
	Decisions       []apiv1.VerdictDecision     `json:"decisions"`
	FindingClasses  []verdictFindingClassDigest `json:"findingClasses"`
}

type implementationHotFile struct {
	Path                        string   `json:"path"`
	PullRequestCount            int      `json:"pullRequestCount"`
	PullRequests                []int    `json:"pullRequests"`
	PullRequestsTruncated       bool     `json:"pullRequestsTruncated,omitempty"`
	RecentConflictCount         int      `json:"recentConflictCount"`
	RecentConflictRuns          []string `json:"recentConflictRuns"`
	RecentConflictRunsTruncated bool     `json:"recentConflictRunsTruncated,omitempty"`
}

type implementationHotFileMap struct {
	OpenPullRequests     int                     `json:"openPullRequests"`
	RecentConflictRuns   int                     `json:"recentConflictRuns"`
	ConflictLookbackDays int                     `json:"conflictLookbackDays"`
	TotalFiles           int                     `json:"totalFiles"`
	Truncated            bool                    `json:"truncated"`
	Files                []implementationHotFile `json:"files"`
}

type implementationContext struct {
	SchemaVersion   string                        `json:"schemaVersion"`
	VerdictTaxonomy reviewerVerdictTaxonomyDigest `json:"reviewerVerdictTaxonomy"`
	HotFileMap      implementationHotFileMap      `json:"hotFileMap"`
	// PriorUnpushedWork, when present, is a previous run's committed-but-
	// never-published diff for the SAME backlog item this run claimed
	// (#3366): work an environmental fault stranded (an egress 403 at
	// local-ci, a daemon restart, a rejected push). Offered as context so the
	// implementer can recover it instead of redoing it. Advisory: the diff
	// was cut against that run's base, which may have moved since.
	PriorUnpushedWork *priorUnpushedWork `json:"priorUnpushedWork,omitempty"`
}

const gatherImplementContextHelp = "Usage: goobers gather-implement-context [path]\n\n" +
	"Emit bounded first-pass implementation context: the shipped merge-review\n" +
	"verdict taxonomy and a hot-file map aggregated from currently-open goober\n" +
	"pull requests plus exact base-sync conflict files journaled in the last 30\n" +
	"days. The result is a workflow-stage artifact carried to implement\n" +
	"through the runner's ordinary contextPointers path. maxHotFiles defaults\n" +
	"to 100 and is capped at 500. Exit codes: 0 = context gathered (an empty\n" +
	"hot-file map is valid), 1 = business/provider/journal error, 2 = usage/IO\n" +
	"error.\n"

func runGatherImplementContext(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("gather-implement-context", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "gather-implement-context")
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
	provider, err := implementationContextProvider(root, repo)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	limit, err := implementationHotFileLimit(providerInput("maxHotFiles", ""))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	ctx, cancel := providerCommandContext()
	defer cancel()
	base := providerInput("base", providerBaseBranch())
	openTouches, err := openPRTouches(ctx, provider, repo, base)
	if err != nil {
		return failProviderStage(stderr, "gather implementation hot-file map", err, implementationContextResultFile)
	}
	recentConflicts, err := recentImplementationConflicts(
		root,
		providerGaggle(),
		time.Now().UTC().Add(-implementationConflictHistoryWindow),
	)
	if err != nil {
		pf(stderr, "error: gather implementation conflict history: %v\n", err)
		return 1
	}

	out := implementationContext{
		SchemaVersion:   "v1",
		VerdictTaxonomy: shippedReviewerVerdictTaxonomy(),
		HotFileMap:      buildImplementationHotFileMap(openTouches, recentConflicts, limit),
	}
	// #3366: offer a prior run's stranded (committed but never published)
	// diff for the same claimed item, if one exists. Best-effort — discovery
	// failure must never fail context gathering.
	if runID := os.Getenv(executor.RunIDEnvVar); runID != "" {
		out.PriorUnpushedWork = latestPriorUnpushedWork(
			root,
			providerGaggle(),
			runID,
			time.Now().UTC().Add(-implementationConflictHistoryWindow),
			stderr,
		)
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		pf(stderr, "error: marshal implementation context: %v\n", err)
		return 1
	}
	resultFile := providerInput("resultFile", implementationContextResultFile)
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	truncation := ""
	if out.HotFileMap.Truncated {
		truncation = " (truncated)"
	}
	priorWork := ""
	if out.PriorUnpushedWork != nil {
		priorWork = fmt.Sprintf(", prior unpushed diff from run %s (%d bytes)",
			out.PriorUnpushedWork.RunID, out.PriorUnpushedWork.DiffBytes)
	}
	pf(stdout, "implementation context: %d open PR(s), %d recent conflict run(s), %d hot file(s)%s%s\n",
		out.HotFileMap.OpenPullRequests,
		out.HotFileMap.RecentConflictRuns,
		len(out.HotFileMap.Files),
		truncation,
		priorWork,
	)
	return 0
}

func implementationContextProvider(root string, repo providers.RepositoryRef) (openPRTouchesProvider, error) {
	provider, err := newProviderForStage(root, repo, false,
		withStageProviderCapability(capability.GitHubPRWrite),
		withStageProviderCache(),
	)
	if err != nil {
		return nil, err
	}
	contextProvider, ok := provider.(openPRTouchesProvider)
	if !ok {
		return nil, fmt.Errorf("gather-implement-context does not support repository provider %q", repo.Provider)
	}
	return contextProvider, nil
}

func implementationHotFileLimit(raw string) (int, error) {
	if raw == "" {
		return defaultImplementationHotFiles, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxImplementationHotFiles {
		return 0, fmt.Errorf("invalid maxHotFiles %q (want an integer from 1 to %d)", raw, maxImplementationHotFiles)
	}
	return n, nil
}

func shippedReviewerVerdictTaxonomy() reviewerVerdictTaxonomyDigest {
	return reviewerVerdictTaxonomyDigest{
		ContractVersion: apiv1.StageContractVersion,
		Decisions: []apiv1.VerdictDecision{
			apiv1.VerdictPass,
			apiv1.VerdictNeedsChanges,
			apiv1.VerdictFail,
		},
		FindingClasses: []verdictFindingClassDigest{
			{Class: apiv1.FindingRebaseNeeded, Meaning: "the base advanced and the pull request must be rebased"},
			{Class: apiv1.FindingConflict, Meaning: "a rebase does not apply cleanly and requires conflict resolution"},
			{Class: apiv1.FindingSubstantive, Meaning: "a defect, regression, drift, or review concern requires a code change"},
			{Class: apiv1.FindingMissingTests, Meaning: "new or changed behavior lacks required test coverage"},
			{Class: apiv1.FindingScopeCreep, Meaning: "changes outside the requested scope must be removed"},
			{Class: apiv1.FindingContractChange, Meaning: "an unauthorized load-bearing contract change requires correction or escalation"},
			{Class: apiv1.FindingCrossPRBlocked, Meaning: "the change is correct in isolation but must wait for named sibling pull requests"},
		},
	}
}

type implementationConflictTouch struct {
	runID string
	files []string
}

type implementationConflictArtifact struct {
	Code             string   `json:"code"`
	ConflictingFiles []string `json:"conflictingFiles"`
}

// recentImplementationConflicts returns, per prior run in the gaggle, the
// files that run's base-sync conflict artifact named since the cutoff.
//
// Cross-run by nature, so it goes through the cross-run journal seam
// (stagejournal.go): the same walk over the instance's run directories when
// the stage has one, and the daemon's gaggle-scoped conflict-touches route
// when it does not (decision 005 R1 / #3880). The route answers with run ids
// and file paths only — none of the prior runs' other journal content crosses
// the boundary.
//
// Failure is fatal to the caller, deliberately: the hot-file map is what makes
// the implementer avoid a known-contended file, and an empty map from a failed
// read is indistinguishable from "nothing is contended".
func recentImplementationConflicts(root, gaggle string, since time.Time) ([]implementationConflictTouch, error) {
	reader, err := stageCrossRunJournal(root, nil)
	if err != nil {
		return nil, err
	}
	runID := os.Getenv(executor.RunIDEnvVar)
	touches, err := reader.ConflictTouches(context.Background(), journalclient.ConflictTouchRequest{
		RunID:  runID,
		Gaggle: gaggle,
		Since:  since,
	})
	if err != nil {
		return nil, err
	}
	conflicts := make([]implementationConflictTouch, 0, len(touches))
	for _, touch := range touches {
		conflicts = append(conflicts, implementationConflictTouch{runID: touch.RunID, files: touch.Files})
	}
	return conflicts, nil
}

// --- prior unpushed work discovery (#3366) --------------------------------

// maxPriorUnpushedDiffInlineBytes bounds the diff carried inline in the
// implementation context, keeping the context artifact itself bounded. The
// full diff always remains addressable in the prior run's journal by the
// digest this section names.
//
// The artifact contract this discovery reads (internal/runner's
// recordUnpushedDiff sidecar) now lives with the discovery itself, in
// internal/journalclient — the same code answers it on this host and inside
// the daemon, so the two can no longer drift.
const maxPriorUnpushedDiffInlineBytes = journalclient.DefaultMaxInlineDiffBytes

// priorUnpushedWork is the implementation-context section describing a prior
// run's stranded diff (#3366).
type priorUnpushedWork struct {
	RunID         string    `json:"runId"`
	Stage         string    `json:"stage"`
	Attempt       int       `json:"attempt"`
	RecordedAt    time.Time `json:"recordedAt"`
	Branch        string    `json:"branch,omitempty"`
	BaseRef       string    `json:"baseRef,omitempty"`
	ItemIDs       []string  `json:"itemIds,omitempty"`
	DiffBytes     int       `json:"diffBytes"`
	DiffDigest    string    `json:"diffDigest,omitempty"`
	Diff          string    `json:"diff,omitempty"`
	DiffTruncated bool      `json:"diffTruncated,omitempty"`
	Note          string    `json:"note"`
}

const priorUnpushedWorkNote = "A previous run on this same backlog item committed this diff but an " +
	"environmental fault prevented publication (#3366) — no branch or PR exists for it. " +
	"Review it and recover what is still valid instead of re-implementing from scratch. " +
	"It was cut against that run's base, which may have moved since: verify it applies " +
	"and still makes sense before reusing it."

// latestPriorUnpushedWork returns the newest committed-but-never-published
// diff a previous run recorded for one of the items THIS run currently
// claims, or nil when there is none. Best-effort throughout: any failure
// (ledger unreadable, a corrupt journal, a refused plane read) warns LOUDLY
// and degrades to nil rather than failing context gathering — the section is
// advisory, not load-bearing, but its absence must never be silent, because
// "no prior work exists" and "we could not find out" produce the same empty
// section and only one of them is true.
//
// The discovery itself runs through the cross-run journal seam
// (stagejournal.go). On the plane the daemon derives the asking run's claimed
// items from its own ledger and ignores anything this caller sends, so a pod
// can only ever learn about work stranded on an item it actually holds
// (decision 005 R1 / #3880).
func latestPriorUnpushedWork(root, gaggle, currentRunID string, since time.Time, stderr io.Writer) *priorUnpushedWork {
	reader, err := stageCrossRunJournal(root, func(msg string) {
		pf(stderr, "warning: prior unpushed work discovery: %s\n", msg)
	})
	if err != nil {
		pf(stderr, "warning: prior unpushed work discovery unavailable: %v\n", err)
		return nil
	}
	request := journalclient.UnpushedWorkRequest{
		RunID:              currentRunID,
		Gaggle:             gaggle,
		Since:              since,
		MaxInlineDiffBytes: maxPriorUnpushedDiffInlineBytes,
	}
	// The item set is the caller's to supply only on the same-host path, where
	// this process can read the ledger directly. On the plane the daemon is
	// the authority and this list is ignored, so it is not even computed.
	if _, isFile := reader.(*journalclient.FileCrossRun); isFile {
		itemIDs, err := claimedItemIDsForRun(layoutFor(root), currentRunID)
		if err != nil {
			pf(stderr, "warning: prior unpushed work discovery: %v\n", err)
			return nil
		}
		if len(itemIDs) == 0 {
			return nil
		}
		request.ItemIDs = itemIDs
	}
	work, err := reader.UnpushedWork(context.Background(), request)
	if err != nil {
		pf(stderr, "warning: prior unpushed work discovery: %v\n", err)
		return nil
	}
	if work == nil {
		return nil
	}
	return &priorUnpushedWork{
		RunID:         work.RunID,
		Stage:         work.Stage,
		Attempt:       work.Attempt,
		RecordedAt:    work.RecordedAt,
		Branch:        work.Branch,
		BaseRef:       work.BaseRef,
		ItemIDs:       work.ItemIDs,
		DiffBytes:     work.DiffBytes,
		DiffDigest:    work.DiffDigest,
		Diff:          work.Diff,
		DiffTruncated: work.DiffTruncated,
		Note:          priorUnpushedWorkNote,
	}
}

type implementationFileEvidence struct {
	openPullRequests   map[int]struct{}
	recentConflictRuns map[string]struct{}
}

func buildImplementationHotFileMap(openTouches []openPRTouch, recentConflicts []implementationConflictTouch, limit int) implementationHotFileMap {
	byPath := make(map[string]*implementationFileEvidence)
	addEvidence := func(path string) *implementationFileEvidence {
		evidence := byPath[path]
		if evidence == nil {
			evidence = &implementationFileEvidence{
				openPullRequests:   make(map[int]struct{}),
				recentConflictRuns: make(map[string]struct{}),
			}
			byPath[path] = evidence
		}
		return evidence
	}
	for _, touch := range openTouches {
		seen := make(map[string]struct{}, len(touch.files))
		for _, path := range touch.files {
			if path == "" {
				continue
			}
			if _, duplicate := seen[path]; duplicate {
				continue
			}
			seen[path] = struct{}{}
			addEvidence(path).openPullRequests[touch.number] = struct{}{}
		}
	}
	for _, conflict := range recentConflicts {
		seen := make(map[string]struct{}, len(conflict.files))
		for _, path := range conflict.files {
			if path == "" {
				continue
			}
			if _, duplicate := seen[path]; duplicate {
				continue
			}
			seen[path] = struct{}{}
			addEvidence(path).recentConflictRuns[conflict.runID] = struct{}{}
		}
	}

	files := make([]implementationHotFile, 0, len(byPath))
	for path, evidence := range byPath {
		openPRs, openTruncated := boundedImplementationPRs(evidence.openPullRequests)
		conflictRuns, conflictsTruncated := boundedImplementationRuns(evidence.recentConflictRuns)
		hotFile := implementationHotFile{
			Path:                        path,
			PullRequestCount:            len(evidence.openPullRequests),
			PullRequests:                openPRs,
			PullRequestsTruncated:       openTruncated,
			RecentConflictCount:         len(evidence.recentConflictRuns),
			RecentConflictRuns:          conflictRuns,
			RecentConflictRunsTruncated: conflictsTruncated,
		}
		files = append(files, hotFile)
	}
	sort.Slice(files, func(i, j int) bool {
		iTotal := files[i].PullRequestCount + files[i].RecentConflictCount
		jTotal := files[j].PullRequestCount + files[j].RecentConflictCount
		if iTotal != jTotal {
			return iTotal > jTotal
		}
		if files[i].PullRequestCount != files[j].PullRequestCount {
			return files[i].PullRequestCount > files[j].PullRequestCount
		}
		if files[i].RecentConflictCount != files[j].RecentConflictCount {
			return files[i].RecentConflictCount > files[j].RecentConflictCount
		}
		return files[i].Path < files[j].Path
	})

	out := implementationHotFileMap{
		OpenPullRequests:     len(openTouches),
		RecentConflictRuns:   len(recentConflicts),
		ConflictLookbackDays: int(implementationConflictHistoryWindow / (24 * time.Hour)),
		TotalFiles:           len(files),
	}
	if limit < 0 {
		limit = 0
	}
	if len(files) > limit {
		out.Truncated = true
		files = files[:limit]
	}
	out.Files = files
	return out
}

func boundedImplementationPRs(prSet map[int]struct{}) ([]int, bool) {
	prs := make([]int, 0, len(prSet))
	for number := range prSet {
		prs = append(prs, number)
	}
	sort.Ints(prs)
	if len(prs) > maxImplementationRefsPerHotFile {
		return prs[:maxImplementationRefsPerHotFile], true
	}
	return prs, false
}

func boundedImplementationRuns(runSet map[string]struct{}) ([]string, bool) {
	runs := make([]string, 0, len(runSet))
	for runID := range runSet {
		runs = append(runs, runID)
	}
	sort.Strings(runs)
	if len(runs) > maxImplementationRefsPerHotFile {
		return runs[:maxImplementationRefsPerHotFile], true
	}
	return runs, false
}
