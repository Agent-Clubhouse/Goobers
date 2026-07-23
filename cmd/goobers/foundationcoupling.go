package main

import (
	"context"
	"fmt"
	"regexp"
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

var unifiedHunkHeaderPattern = regexp.MustCompile(`(?m)^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@`)

type pullRequestDiffProfile struct {
	pr        providers.PullRequestSummary
	files     []providers.ChangedFile
	byPath    map[string]providers.ChangedFile
	magnitude int
}

type foundationCoupling struct {
	dependent  providers.PullRequestSummary
	foundation providers.PullRequestSummary
	files      []string
	score      int
}

func loadFoundationCouplings(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef, prs []providers.PullRequestSummary, blocked map[int]bool) ([]foundationCoupling, error) {
	if len(prs) < 2 {
		return nil, nil
	}
	profiles := make([]pullRequestDiffProfile, 0, len(prs))
	for _, pr := range prs {
		files, err := provider.PullRequestFiles(ctx, repo, strconv.Itoa(pr.Number))
		if err != nil {
			return nil, fmt.Errorf("list files for PR #%d: %w", pr.Number, err)
		}
		profiles = append(profiles, newPullRequestDiffProfile(pr, files))
	}
	return detectFoundationCouplings(profiles, blocked), nil
}

func newPullRequestDiffProfile(pr providers.PullRequestSummary, files []providers.ChangedFile) pullRequestDiffProfile {
	profile := pullRequestDiffProfile{
		pr:     pr,
		files:  files,
		byPath: make(map[string]providers.ChangedFile, len(files)),
	}
	for _, file := range files {
		profile.byPath[file.Path] = file
		profile.magnitude += changedFileMagnitude(file)
	}
	return profile
}

func detectFoundationCouplings(profiles []pullRequestDiffProfile, blocked map[int]bool) []foundationCoupling {
	var couplings []foundationCoupling
	for _, dependent := range profiles {
		var candidates []foundationCoupling
		for _, foundation := range profiles {
			if foundation.pr.Number == dependent.pr.Number ||
				foundation.pr.Draft ||
				blocked[foundation.pr.Number] ||
				hasAnyLabel(foundation.pr.Labels, []string{mergeDemotedLabel, remediationEscalatedLabel}) {
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
		dependentFile, ok := dependent.byPath[foundationFile.Path]
		if !ok ||
			!substantiallyRewrites(foundationFile) ||
			substantiallyRewrites(dependentFile) ||
			changedFileMagnitude(foundationFile) < foundationDominanceFactor*changedFileMagnitude(dependentFile) {
			continue
		}
		shared = append(shared, foundationFile.Path)
	}
	sort.Strings(shared)
	return shared
}

func changedFileMagnitude(file providers.ChangedFile) int {
	magnitude := file.Additions + file.Deletions
	if strings.EqualFold(file.Status, "removed") && magnitude < foundationRewriteMinLines {
		return foundationRewriteMinLines
	}
	if magnitude == 0 {
		return 1
	}
	return magnitude
}

func substantiallyRewrites(file providers.ChangedFile) bool {
	if strings.EqualFold(file.Status, "removed") {
		return true
	}
	if file.Deletions < foundationRewriteMinLines {
		return false
	}
	if file.Additions*10 <= file.Deletions {
		return true
	}
	oldExtent, newExtent, startsAtBeginning, ok := diffHunkExtents(file.Patch)
	if !ok || oldExtent < foundationRewriteMinLines {
		return false
	}
	if startsAtBeginning && newExtent*10 <= oldExtent {
		return true
	}
	return file.Deletions*2 >= oldExtent
}

func diffHunkExtents(patch string) (oldExtent, newExtent int, startsAtBeginning, ok bool) {
	matches := unifiedHunkHeaderPattern.FindAllStringSubmatch(patch, -1)
	if len(matches) == 0 {
		return 0, 0, false, false
	}
	minOldStart, minNewStart := int(^uint(0)>>1), int(^uint(0)>>1)
	for _, match := range matches {
		oldStart, oldCount, parsed := parseHunkRange(match[1], match[2])
		if !parsed {
			return 0, 0, false, false
		}
		newStart, newCount, parsed := parseHunkRange(match[3], match[4])
		if !parsed {
			return 0, 0, false, false
		}
		minOldStart = min(minOldStart, oldStart)
		minNewStart = min(minNewStart, newStart)
		oldExtent = max(oldExtent, hunkEnd(oldStart, oldCount))
		newExtent = max(newExtent, hunkEnd(newStart, newCount))
	}
	return oldExtent, newExtent, minOldStart <= 1 && minNewStart <= 1, true
}

func parseHunkRange(startText, countText string) (start, count int, ok bool) {
	start, err := strconv.Atoi(startText)
	if err != nil {
		return 0, 0, false
	}
	count = 1
	if countText != "" {
		count, err = strconv.Atoi(countText)
		if err != nil {
			return 0, 0, false
		}
	}
	return start, count, true
}

func hunkEnd(start, count int) int {
	if count == 0 {
		return start
	}
	return start + count - 1
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
