package main

import (
	"encoding/json"
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
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
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
	fs := flag.NewFlagSet("gather-implement-context", flag.ContinueOnError)
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
	token, err := providerToken(capability.GitHubPRWrite)
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
	provider := newGitHubProvider(token)
	base := providerInput("base", "main")
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
	pf(stdout, "implementation context: %d open PR(s), %d recent conflict run(s), %d hot file(s)%s\n",
		out.HotFileMap.OpenPullRequests,
		out.HotFileMap.RecentConflictRuns,
		len(out.HotFileMap.Files),
		truncation,
	)
	return 0
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

func recentImplementationConflicts(root, gaggle string, since time.Time) ([]implementationConflictTouch, error) {
	layout := layoutFor(root)
	if gaggle != "" {
		layout = layout.ForGaggle(gaggle)
	}
	runDirs, err := layout.RunDirs()
	if err != nil {
		return nil, err
	}

	byRun := make(map[string]map[string]struct{})
	for _, runsDir := range runDirs {
		entries, err := os.ReadDir(runsDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read runs directory %s: %w", runsDir, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			reader, err := journal.OpenRead(filepath.Join(runsDir, entry.Name()))
			if err != nil {
				continue
			}
			events, err := reader.Events()
			if err != nil {
				return nil, err
			}
			for _, event := range events {
				if !event.KnownSchema() ||
					event.Type != journal.EventArtifactRecorded ||
					event.Ref == nil ||
					event.Time.Before(since) ||
					!strings.HasSuffix(event.Name, "/base-sync-conflict.json") {
					continue
				}
				data, err := reader.ArtifactBytes(*event.Ref)
				if err != nil {
					return nil, err
				}
				var artifact implementationConflictArtifact
				if err := json.Unmarshal(data, &artifact); err != nil {
					return nil, fmt.Errorf("decode conflict artifact for run %s: %w", entry.Name(), err)
				}
				if artifact.Code != "base_sync_conflict" || len(artifact.ConflictingFiles) == 0 {
					continue
				}
				files := byRun[entry.Name()]
				if files == nil {
					files = make(map[string]struct{})
					byRun[entry.Name()] = files
				}
				for _, path := range artifact.ConflictingFiles {
					if path != "" {
						files[path] = struct{}{}
					}
				}
			}
		}
	}
	runIDs := make([]string, 0, len(byRun))
	for runID := range byRun {
		runIDs = append(runIDs, runID)
	}
	sort.Strings(runIDs)
	conflicts := make([]implementationConflictTouch, 0, len(runIDs))
	for _, runID := range runIDs {
		files := make([]string, 0, len(byRun[runID]))
		for path := range byRun[runID] {
			files = append(files, path)
		}
		sort.Strings(files)
		conflicts = append(conflicts, implementationConflictTouch{runID: runID, files: files})
	}
	return conflicts, nil
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
