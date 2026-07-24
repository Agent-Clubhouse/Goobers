package main

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

const (
	foundationRewriteMinLines = 100
	foundationDominanceFactor = 2
)

type fileLineCounts struct {
	before int
	after  int
}

type pullRequestDiffProfile struct {
	pr         providers.PullRequestSummary
	files      []providers.ChangedFile
	byPath     map[string]providers.ChangedFile
	lineCounts map[string]fileLineCounts
	magnitude  int
}

type foundationCoupling struct {
	dependent  providers.PullRequestSummary
	foundation providers.PullRequestSummary
	files      []string
	score      int
}

func loadFoundationCouplings(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	dependents []providers.PullRequestSummary,
	openPRs []providers.PullRequestSummary,
	blocked map[int]bool,
) ([]foundationCoupling, []string, error) {
	if len(dependents) == 0 || len(openPRs) < 2 {
		return nil, nil, nil
	}
	profiles := make([]pullRequestDiffProfile, 0, len(openPRs))
	profileIndexes := make(map[int]int, len(openPRs))
	for _, pr := range openPRs {
		files, err := provider.PullRequestFiles(ctx, repo, strconv.Itoa(pr.Number))
		if err != nil {
			return nil, nil, fmt.Errorf("list files for PR #%d: %w", pr.Number, err)
		}
		profileIndexes[pr.Number] = len(profiles)
		profiles = append(profiles, newPullRequestDiffProfile(pr, files))
	}
	dependentProfiles := make([]pullRequestDiffProfile, 0, len(dependents))
	dependentPaths := make(map[string]bool)
	for _, dependent := range dependents {
		index, ok := profileIndexes[dependent.Number]
		if !ok {
			continue
		}
		dependentProfiles = append(dependentProfiles, profiles[index])
		for _, file := range profiles[index].files {
			dependentPaths[file.Path] = true
		}
	}
	if len(dependentProfiles) == 0 {
		return nil, nil, nil
	}

	var warnings []string
	foundationBlocked := make(map[int]bool, len(blocked))
	for number, isBlocked := range blocked {
		if isBlocked {
			foundationBlocked[number] = true
		}
	}
	for _, pr := range openPRs {
		if pr.Draft || foundationBlocked[pr.Number] {
			continue
		}
		escalated, err := escalationStillBlocks(ctx, provider, repo, pr)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve merge-escalation state for foundation PR #%d: %w", pr.Number, err)
		}
		if escalated {
			foundationBlocked[pr.Number] = true
			continue
		}
		demoted, err := demotionStillHolds(ctx, provider, repo, pr)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"could not resolve merge-demotion state for foundation PR #%d (%v); treating it as not demoted",
				pr.Number, err,
			))
			continue
		}
		if demoted {
			foundationBlocked[pr.Number] = true
		}
	}

	contentCache := make(map[string][]byte)
	for i := range profiles {
		for _, file := range profiles[i].files {
			if !dependentPaths[file.Path] ||
				!strings.EqualFold(file.Status, "modified") ||
				file.Deletions < foundationRewriteMinLines {
				continue
			}
			counts, err := repositoryFileLineCounts(ctx, provider, repo, profiles[i].pr, file, contentCache)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf(
					"could not determine line counts for PR #%d file %s: %v; skipping it",
					profiles[i].pr.Number, file.Path, err,
				))
				continue
			}
			profiles[i].lineCounts[file.Path] = counts
		}
	}
	for i := range dependentProfiles {
		dependentProfiles[i] = profiles[profileIndexes[dependentProfiles[i].pr.Number]]
	}
	return detectFoundationCouplings(dependentProfiles, profiles, foundationBlocked), warnings, nil
}

func newPullRequestDiffProfile(pr providers.PullRequestSummary, files []providers.ChangedFile) pullRequestDiffProfile {
	profile := pullRequestDiffProfile{
		pr:         pr,
		files:      files,
		byPath:     make(map[string]providers.ChangedFile, len(files)),
		lineCounts: make(map[string]fileLineCounts),
	}
	for _, file := range files {
		profile.byPath[file.Path] = file
		if removal, ok := renamedFileRemoval(file); ok {
			profile.byPath[removal.Path] = removal
		}
		profile.magnitude += changedFileMagnitude(file)
	}
	return profile
}

func repositoryFileLineCounts(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	pr providers.PullRequestSummary,
	file providers.ChangedFile,
	cache map[string][]byte,
) (fileLineCounts, error) {
	key := pr.HeadSHA + "\x00" + file.Path
	afterContent, ok := cache[key]
	if !ok {
		var err error
		afterContent, err = provider.RepositoryFileContent(ctx, repo, file.Path, pr.HeadSHA)
		if err != nil {
			return fileLineCounts{}, fmt.Errorf("read head %s: %w", pr.HeadSHA, err)
		}
		cache[key] = afterContent
	}
	after := contentLineCount(afterContent)
	before := after - file.Additions + file.Deletions
	if before < 0 {
		return fileLineCounts{}, fmt.Errorf(
			"invalid line counts at head %s: %d lines with %d additions and %d deletions",
			pr.HeadSHA, after, file.Additions, file.Deletions,
		)
	}
	return fileLineCounts{before: before, after: after}, nil
}

func contentLineCount(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	lines := bytes.Count(content, []byte{'\n'})
	if content[len(content)-1] != '\n' {
		lines++
	}
	return lines
}

func detectFoundationCouplings(dependents, foundations []pullRequestDiffProfile, blocked map[int]bool) []foundationCoupling {
	var couplings []foundationCoupling
	for _, dependent := range dependents {
		var candidates []foundationCoupling
		for _, foundation := range foundations {
			if foundation.pr.Number == dependent.pr.Number ||
				foundation.pr.Draft ||
				blocked[foundation.pr.Number] {
				continue
			}
			shared := foundationRewriteFiles(dependent, foundation)
			if len(shared) == 0 ||
				foundation.magnitude < foundationDominanceFactor*dependent.magnitude {
				continue
			}
			candidates = append(candidates, foundationCoupling{
				dependent: dependent.pr, foundation: foundation.pr,
				files: shared, score: foundation.magnitude,
			})
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].score != candidates[j].score {
				return candidates[i].score > candidates[j].score
			}
			return candidates[i].foundation.Number < candidates[j].foundation.Number
		})
		if len(candidates) == 0 {
			continue
		}
		if len(candidates) > 1 && candidates[0].score < foundationDominanceFactor*candidates[1].score {
			continue
		}
		couplings = append(couplings, candidates[0])
	}
	sort.Slice(couplings, func(i, j int) bool {
		return couplings[i].dependent.Number < couplings[j].dependent.Number
	})
	return couplings
}

func foundationRewriteFiles(dependent, foundation pullRequestDiffProfile) []string {
	var shared []string
	for _, foundationFile := range foundation.files {
		rewriteFile := foundationFile
		sharedPath := foundationFile.Path
		if removal, ok := renamedFileRemoval(foundationFile); ok {
			rewriteFile = removal
			sharedPath = removal.Path
		}
		dependentFile, ok := dependent.byPath[sharedPath]
		if !ok {
			continue
		}
		foundationRewrite, foundationKnown := substantiallyRewrites(
			rewriteFile, foundation.lineCounts[foundationFile.Path],
			lineCountsKnown(rewriteFile, foundation.lineCounts, foundationFile.Path),
		)
		dependentRewrite, dependentKnown := substantiallyRewrites(
			dependentFile, dependent.lineCounts[sharedPath],
			lineCountsKnown(dependentFile, dependent.lineCounts, sharedPath),
		)
		if !foundationKnown ||
			!dependentKnown ||
			!foundationRewrite ||
			dependentRewrite ||
			changedFileMagnitude(rewriteFile) < foundationDominanceFactor*changedFileMagnitude(dependentFile) {
			continue
		}
		shared = append(shared, sharedPath)
	}
	sort.Strings(shared)
	return shared
}

func lineCountsKnown(file providers.ChangedFile, counts map[string]fileLineCounts, path string) bool {
	if strings.EqualFold(file.Status, "removed") || file.Deletions < foundationRewriteMinLines {
		return true
	}
	_, ok := counts[path]
	return ok
}

func renamedFileRemoval(file providers.ChangedFile) (providers.ChangedFile, bool) {
	if !strings.EqualFold(file.Status, "renamed") ||
		file.PreviousPath == "" ||
		file.PreviousPath == file.Path {
		return providers.ChangedFile{}, false
	}
	file.Path = file.PreviousPath
	file.Status = "removed"
	return file, true
}

func changedFileMagnitude(file providers.ChangedFile) int {
	if removal, ok := renamedFileRemoval(file); ok {
		file = removal
	}
	magnitude := file.Additions + file.Deletions
	if strings.EqualFold(file.Status, "removed") && magnitude < foundationRewriteMinLines {
		return foundationRewriteMinLines
	}
	if magnitude == 0 {
		return 1
	}
	return magnitude
}

func substantiallyRewrites(file providers.ChangedFile, lines fileLineCounts, countsKnown bool) (bool, bool) {
	if strings.EqualFold(file.Status, "removed") {
		return true, true
	}
	if file.Deletions < foundationRewriteMinLines {
		return false, true
	}
	if !countsKnown {
		return false, false
	}
	if lines.before < foundationRewriteMinLines {
		return false, true
	}
	if lines.after*10 <= lines.before {
		return true, true
	}
	return file.Deletions*2 >= lines.before, true
}

func flagFoundationCoupling(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, coupling foundationCoupling, existingBlockers []int) (bool, error) {
	for _, blocker := range existingBlockers {
		if blocker == coupling.foundation.Number {
			return false, nil
		}
	}
	blockers := append(append([]int(nil), existingBlockers...), coupling.foundation.Number)
	sort.Ints(blockers)
	reason := fmt.Sprintf(
		"foundation-coupled to PR #%d, which substantially rewrites or deletes shared file(s) %s",
		coupling.foundation.Number, markdownPaths(coupling.files),
	)
	state := blockedOnSiblingState{
		Blockers: blockers, Reason: reason,
		HeadSHA: coupling.dependent.HeadSHA, BaseSHA: coupling.dependent.BaseSHA,
		RecordedAt: time.Now().UTC(),
	}
	payload, err := blockedOnSiblingComment(state)
	if err != nil {
		return false, err
	}
	comment := fmt.Sprintf(
		"**Foundation-coupled hold**\n\nPR #%d substantially rewrites or deletes %s, which this pull request also changes. This narrower PR is parked until that foundation closes so remediation does not retry a patch against a structure that is about to move. If the foundation lands, post-merge overlap handling can re-derive this work against the new base.\n\n%s",
		coupling.foundation.Number, markdownPaths(coupling.files), payload,
	)
	var addLabels []string
	if !hasAnyLabel(coupling.dependent.Labels, []string{blockedOnSiblingLabel}) {
		addLabels = []string{blockedOnSiblingLabel}
	}
	if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
		Repository: repo,
		ID:         strconv.Itoa(coupling.dependent.Number),
		AddLabels:  addLabels,
		Comment:    comment,
	}); err != nil {
		return false, fmt.Errorf("flag PR #%d behind foundation PR #%d: %w", coupling.dependent.Number, coupling.foundation.Number, err)
	}
	return true, nil
}

func markdownPaths(paths []string) string {
	quoted := make([]string, 0, len(paths))
	for _, path := range paths {
		quoted = append(quoted, "`"+strings.ReplaceAll(path, "`", "\\`")+"`")
	}
	return strings.Join(quoted, ", ")
}
