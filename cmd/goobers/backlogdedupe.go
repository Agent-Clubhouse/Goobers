package main

import (
	"encoding/json"
	"flag"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

const (
	defaultDedupeCandidates = 20
	maxDedupeCandidates     = 100
	dedupeArtifactVersion   = "v1"
)

var (
	dedupeWordPattern          = regexp.MustCompile(`[a-z0-9]+`)
	dedupeLinkPattern          = regexp.MustCompile(`https?://[^\s<>()\[\]{}"'` + "`" + `]+`)
	dedupeExternalRefPattern   = regexp.MustCompile(`(?i)\b[A-Z][A-Z0-9]{1,9}-[0-9]+\b`)
	dedupeTaxonomyTitlePattern = regexp.MustCompile(`(?i)^\s*(?:\[[^\]]+\]\s*)?([A-Z][A-Z0-9]{1,9})-[0-9]+\b`)
)

var dedupeStopWords = map[string]bool{
	"about": true, "acceptance": true, "add": true, "after": true, "also": true,
	"and": true, "are": true, "body": true, "criteria": true, "for": true,
	"from": true, "has": true, "have": true, "into": true, "issue": true,
	"must": true, "not": true, "only": true, "should": true, "that": true,
	"the": true, "their": true, "this": true, "using": true, "when": true,
	"where": true, "with": true,
}

const backlogDedupeHelp = "Usage: goobers backlog-dedupe [path]\n\n" +
	"Surface ranked likely-duplicate pairs for the current curation run. The\n" +
	"command compares this run's claimed issues against every open backlog item\n" +
	"using title/body similarity, shared closing references, external references,\n" +
	"and links. It writes a structured candidate artifact for curator judgment;\n" +
	"it never changes or closes an issue. A candidate's closeEligibleId is present\n" +
	"only when its newer issue belongs to this run's claimed, trusted batch;\n" +
	"unclaimed comparison issues are read-only evidence.\n\n" +
	"The maxCandidates stage input defaults to 20 and must be between 1 and 100.\n\n" +
	"Exit codes: 0 = candidate artifact written, 1 = config/credential/provider\n" +
	"error, 2 = usage error.\n"

type dedupeCandidateItem struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	URL     string   `json:"url,omitempty"`
	Labels  []string `json:"labels,omitempty"`
	Claimed bool     `json:"claimed"`
}

type dedupeSignals struct {
	TitleSimilarity          int      `json:"titleSimilarity"`
	BodySimilarity           int      `json:"bodySimilarity"`
	SharedClosingReferences  []string `json:"sharedClosingReferences,omitempty"`
	SharedExternalReferences []string `json:"sharedExternalReferences,omitempty"`
	SharedLinks              []string `json:"sharedLinks,omitempty"`
}

type dedupeCandidate struct {
	Rank            int                 `json:"rank"`
	Score           int                 `json:"score"`
	Older           dedupeCandidateItem `json:"older"`
	Newer           dedupeCandidateItem `json:"newer"`
	CloseEligibleID string              `json:"closeEligibleId,omitempty"`
	Signals         dedupeSignals       `json:"signals"`
}

type dedupeCandidateArtifact struct {
	Version        string            `json:"version"`
	ScannedItems   int               `json:"scannedItems"`
	ClaimedItems   int               `json:"claimedItems"`
	CandidateCount int               `json:"candidateCount"`
	TotalMatches   int               `json:"totalMatches"`
	Truncated      bool              `json:"truncated"`
	ClaimedIDs     []string          `json:"claimedIds"`
	Candidates     []dedupeCandidate `json:"candidates"`
}

func runBacklogDedupe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backlog-dedupe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "backlog-dedupe")
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

	maxCandidates := defaultDedupeCandidates
	if raw := providerInput("maxCandidates", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxDedupeCandidates {
			pf(stderr, "error: invalid maxCandidates %q (want an integer between 1 and %d)\n", raw, maxDedupeCandidates)
			return 1
		}
		maxCandidates = n
	}

	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layoutFor(root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		pf(stderr, "error: open claim ledger: %v\n", err)
		return 1
	}
	entries := ledger.ForRunAll(runID)
	claimedIDs := make([]string, 0, len(entries))
	claimed := make(map[string]bool, len(entries))
	for _, entry := range entries {
		claimedIDs = append(claimedIDs, entry.ItemID)
		claimed[entry.ItemID] = true
	}

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	token, err := providerToken(capability.GitHubIssuesWrite)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	issueProvider := newGitHubProvider(token, apiReadCacheOption(root))
	ctx, cancel := providerCommandContext()
	defer cancel()

	items, err := issueProvider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository:  repo,
		State:       "open",
		OldestFirst: true,
	})
	if err != nil {
		return failProviderStage(stderr, "list open work items for dedupe", err, "dedupe-candidates.json")
	}
	openItems := items[:0]
	for _, item := range items {
		if item.State != "" && !strings.EqualFold(item.State, "open") {
			continue
		}
		openItems = append(openItems, item)
	}

	candidates := surfaceDuplicateCandidates(openItems, claimed)
	totalMatches := len(candidates)
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}
	for i := range candidates {
		candidates[i].Rank = i + 1
	}
	if candidates == nil {
		candidates = []dedupeCandidate{}
	}
	artifact := dedupeCandidateArtifact{
		Version:        dedupeArtifactVersion,
		ScannedItems:   len(openItems),
		ClaimedItems:   len(claimedIDs),
		CandidateCount: len(candidates),
		TotalMatches:   totalMatches,
		Truncated:      totalMatches > len(candidates),
		ClaimedIDs:     claimedIDs,
		Candidates:     candidates,
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		pf(stderr, "error: marshal dedupe candidates: %v\n", err)
		return 1
	}
	resultFile := providerInput("resultFile", "dedupe-candidates.json")
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "surfaced %d likely-duplicate candidate pair(s) from %d open item(s)\n", len(candidates), len(openItems))
	return 0
}

func surfaceDuplicateCandidates(items []providers.WorkItem, claimed map[string]bool) []dedupeCandidate {
	claimedItems := make([]providers.WorkItem, 0, len(claimed))
	for _, item := range items {
		if claimed[item.ID] {
			claimedItems = append(claimedItems, item)
		}
	}
	internalRefPrefixes := internalTaxonomyPrefixes(items)

	var candidates []dedupeCandidate
	seen := make(map[string]bool)
	for _, claimedItem := range claimedItems {
		for _, other := range items {
			if claimedItem.ID == other.ID {
				continue
			}
			older, newer := orderDedupePair(claimedItem, other)
			key := older.ID + "\x00" + newer.ID
			if seen[key] {
				continue
			}
			seen[key] = true

			signals := duplicateSignals(older, newer, internalRefPrefixes)
			score, likely := duplicateCandidateScore(signals)
			if !likely {
				continue
			}
			candidate := dedupeCandidate{
				Score:   score,
				Older:   candidateItem(older, claimed[older.ID]),
				Newer:   candidateItem(newer, claimed[newer.ID]),
				Signals: signals,
			}
			if claimed[newer.ID] {
				candidate.CloseEligibleID = newer.ID
			}
			candidates = append(candidates, candidate)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if candidates[i].Older.ID != candidates[j].Older.ID {
			return workItemIDLess(candidates[i].Older.ID, candidates[j].Older.ID)
		}
		return workItemIDLess(candidates[i].Newer.ID, candidates[j].Newer.ID)
	})
	return candidates
}

func duplicateSignals(a, b providers.WorkItem, internalRefPrefixes map[string]bool) dedupeSignals {
	return dedupeSignals{
		TitleSimilarity:          dedupeTextSimilarity(a.Title, b.Title),
		BodySimilarity:           dedupeTextSimilarity(a.Body, b.Body),
		SharedClosingReferences:  sharedStrings(closingIssueNumbers(a.Body), closingIssueNumbers(b.Body), "#"),
		SharedExternalReferences: sharedStrings(extractExternalReferences(a.Title+"\n"+a.Body, internalRefPrefixes), extractExternalReferences(b.Title+"\n"+b.Body, internalRefPrefixes), ""),
		SharedLinks:              sharedStrings(extractDedupeLinks(a.Body), extractDedupeLinks(b.Body), ""),
	}
}

func duplicateCandidateScore(signals dedupeSignals) (int, bool) {
	textScore := (2*signals.TitleSimilarity + signals.BodySimilarity) / 3
	likely := signals.TitleSimilarity >= 55 ||
		signals.BodySimilarity >= 65 ||
		(signals.TitleSimilarity >= 35 && signals.BodySimilarity >= 35)
	score := max(textScore, max(signals.TitleSimilarity, signals.BodySimilarity))
	if len(signals.SharedLinks) > 0 {
		score = max(score, 90)
		likely = true
	}
	if len(signals.SharedExternalReferences) > 0 {
		score = max(score, 95)
		likely = true
	}
	if len(signals.SharedClosingReferences) > 0 {
		score = 100
		likely = true
	}
	return score, likely
}

func dedupeTextSimilarity(a, b string) int {
	left := dedupeTokens(a)
	right := dedupeTokens(b)
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	shared := 0
	for token := range left {
		if right[token] {
			shared++
		}
	}
	return 200 * shared / (len(left) + len(right))
}

func dedupeTokens(text string) map[string]bool {
	tokens := make(map[string]bool)
	for _, token := range dedupeWordPattern.FindAllString(strings.ToLower(text), -1) {
		if len(token) < 3 || dedupeStopWords[token] || allDigits(token) {
			continue
		}
		tokens[dedupeStem(token)] = true
	}
	return tokens
}

func dedupeStem(token string) string {
	switch {
	case len(token) > 5 && strings.HasSuffix(token, "ies"):
		return strings.TrimSuffix(token, "ies") + "y"
	case len(token) > 5 && strings.HasSuffix(token, "ing"):
		return strings.TrimSuffix(token, "ing")
	case len(token) > 4 && strings.HasSuffix(token, "ed"):
		return strings.TrimSuffix(token, "ed")
	case len(token) > 4 && strings.HasSuffix(token, "es"):
		return strings.TrimSuffix(token, "es")
	case len(token) > 4 && strings.HasSuffix(token, "s") &&
		!strings.HasSuffix(token, "ss") && !strings.HasSuffix(token, "us"):
		return strings.TrimSuffix(token, "s")
	default:
		return token
	}
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func internalTaxonomyPrefixes(items []providers.WorkItem) map[string]bool {
	counts := make(map[string]int)
	for _, item := range items {
		match := dedupeTaxonomyTitlePattern.FindStringSubmatch(item.Title)
		if len(match) == 2 {
			counts[strings.ToUpper(match[1])]++
		}
	}
	prefixes := make(map[string]bool)
	for prefix, count := range counts {
		if count > 1 {
			prefixes[prefix] = true
		}
	}
	return prefixes
}

func extractExternalReferences(text string, internalPrefixes map[string]bool) []string {
	matches := dedupeExternalRefPattern.FindAllString(text, -1)
	external := matches[:0]
	for _, match := range matches {
		match = strings.ToUpper(match)
		prefix, _, ok := strings.Cut(match, "-")
		if !ok || internalPrefixes[prefix] {
			continue
		}
		external = append(external, match)
	}
	return distinctSortedStrings(external)
}

func extractDedupeLinks(text string) []string {
	var links []string
	for _, raw := range dedupeLinkPattern.FindAllString(text, -1) {
		raw = strings.TrimRight(raw, ".,;:!?")
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		u.Fragment = ""
		u.Path = strings.TrimSuffix(u.Path, "/")
		links = append(links, u.String())
	}
	return distinctSortedStrings(links)
}

func sharedStrings(a, b []string, prefix string) []string {
	right := make(map[string]bool, len(b))
	for _, value := range b {
		right[value] = true
	}
	var shared []string
	for _, value := range a {
		if right[value] {
			shared = append(shared, prefix+value)
		}
	}
	return distinctSortedStrings(shared)
}

func distinctSortedStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func candidateItem(item providers.WorkItem, claimed bool) dedupeCandidateItem {
	labels := append([]string(nil), item.Labels...)
	sort.Strings(labels)
	return dedupeCandidateItem{
		ID:      item.ID,
		Title:   item.Title,
		URL:     item.URL,
		Labels:  labels,
		Claimed: claimed,
	}
}

func orderDedupePair(a, b providers.WorkItem) (providers.WorkItem, providers.WorkItem) {
	if workItemIDLess(a.ID, b.ID) {
		return a, b
	}
	return b, a
}

func workItemIDLess(a, b string) bool {
	ai, aOK := parseWorkItemID(a)
	bi, bOK := parseWorkItemID(b)
	if aOK && bOK {
		return ai < bi
	}
	return a < b
}
