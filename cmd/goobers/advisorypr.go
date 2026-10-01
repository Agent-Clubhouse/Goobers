package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

const advisorySelectHelp = "Usage: goobers advisory-pr-select [path]\n\nSelect one open PR for a private-disposition advisory review. Input: reviewType.\n"
const advisoryPublishHelp = "Usage: goobers advisory-pr-publish [path]\n\nPublish a strict advisory reviewer artifact or record a permanent private skip. Inputs: reviewType, reviewerStage, selectionStage.\n"
const advisoryResetHelp = "Usage: goobers advisory-pr-reset --gaggle NAME --owner OWNER --repo REPO --review-type TYPE --pr NUMBER [path]\n\nExplicitly clear one private advisory disposition on the local instance.\n"

const (
	advisorySelectionFile = "advisory-selection.json"
	advisoryReviewFile    = "advisory-review.json"
	advisoryResultFile    = "advisory-result.json"
	advisorySchema        = "goobers.dev/advisory-pr-review/v1"
)

var advisoryTypePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type advisorySelection struct {
	Repository      string                  `json:"repository"`
	ReviewType      string                  `json:"reviewType"`
	SelectedNumber  string                  `json:"selectedNumber"`
	SelectedHeadSHA string                  `json:"selectedHeadSha"`
	URL             string                  `json:"url"`
	Body            string                  `json:"body"`
	Draft           bool                    `json:"draft"`
	Files           []providers.ChangedFile `json:"files"`
}

type advisoryReview struct {
	Schema     string `json:"schema"`
	ReviewType string `json:"reviewType"`
	Number     int    `json:"number"`
	HeadSHA    string `json:"headSha"`
	Decision   string `json:"decision"`
	Comment    string `json:"comment"`
}

type advisoryDisposition struct {
	SkippedAt time.Time `json:"skippedAt,omitempty"`
	HeadSHA   string    `json:"headSha,omitempty"`
	Receipts  []string  `json:"receipts,omitempty"`
}

func advisoryType() (string, error) {
	value := providerInput("reviewType", "")
	if !advisoryTypePattern.MatchString(value) {
		return "", fmt.Errorf("reviewType must be a lower-case name of at most 63 letters, digits, or hyphens")
	}
	return value, nil
}

func advisoryScope(repo providers.RepositoryRef) string {
	return repo.Owner + "/" + repo.Name
}

func advisoryKey(repo providers.RepositoryRef, reviewType string, number int) string {
	return advisoryKeyFor(providerGaggle(), repo, reviewType, number)
}

func advisoryKeyFor(gaggle string, repo providers.RepositoryRef, reviewType string, number int) string {
	sum := sha256.Sum256([]byte(gaggle + "\x00" + advisoryScope(repo) + "\x00" + reviewType + "\x00" + strconv.Itoa(number)))
	return stateclient.AdvisoryPRKey(hex.EncodeToString(sum[:]))
}

func runAdvisoryPRReset(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("advisory-pr-reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "advisory-pr-reset")
	gaggle := fs.String("gaggle", "", "gaggle name")
	owner := fs.String("owner", "", "GitHub owner")
	repoName := fs.String("repo", "", "GitHub repository")
	reviewType := fs.String("review-type", "", "advisory review type")
	number := fs.Int("pr", 0, "pull request number")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 || *gaggle == "" || *owner == "" || *repoName == "" ||
		!advisoryTypePattern.MatchString(*reviewType) || *number <= 0 {
		fs.Usage()
		return 2
	}
	if os.Getenv(executor.RunIDEnvVar) != "" {
		pf(stderr, "error: advisory-pr-reset is an operator command, not a workflow stage\n")
		return 1
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	l := instance.NewLayout(root)
	store, err := fileStateStore(l)
	if err != nil {
		pf(stderr, "error: open private advisory state: %v\n", err)
		return 1
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: *owner, Name: *repoName}
	key := advisoryKeyFor(*gaggle, repo, *reviewType, *number)
	err = store.Update(context.Background(), key, "advisory-pr.operator-reset", func(value stateclient.Value) ([]byte, bool, error) {
		if len(value.Data) == 0 {
			return nil, false, nil
		}
		return []byte("{}"), true, nil
	})
	if err != nil {
		pf(stderr, "error: reset advisory disposition: %v\n", err)
		return 1
	}
	pf(stdout, "reset private %s advisory disposition for %s/%s PR #%d in gaggle %s\n", *reviewType, *owner, *repoName, *number, *gaggle)
	return 0
}

func advisoryClaimKey(repo providers.RepositoryRef, reviewType string, number int) claimsclient.Key {
	return claimsclient.Key{
		Gaggle: providerGaggle(), Provider: string(providers.ProviderGitHub),
		ExternalID: "advisory-pr/" + advisoryScope(repo) + "/" + reviewType + "/" + strconv.Itoa(number),
	}
}

func advisoryStore(root string) (stateclient.Store, error) {
	l := layoutFor(root)
	if !statePlaneSelected() {
		if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
			return nil, err
		}
	}
	return openStageStateStore(l)
}

func readAdvisoryDisposition(ctx context.Context, store stateclient.Store, key string) (advisoryDisposition, error) {
	value, err := store.Get(ctx, key)
	if err != nil || len(value.Data) == 0 {
		return advisoryDisposition{}, err
	}
	var record advisoryDisposition
	if err := json.Unmarshal(value.Data, &record); err != nil {
		return record, fmt.Errorf("decode private advisory disposition: %w", err)
	}
	return record, nil
}

func runAdvisoryPRSelect(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("advisory-pr-select", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "advisory-pr-select")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}
	reviewType, err := advisoryType()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	runID, workflow, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if repo.Provider != providers.ProviderGitHub {
		pf(stderr, "error: advisory PR review currently requires GitHub\n")
		return 1
	}
	provider, err := newProviderForStageAs[*providers.GitHubProvider](root, repo, true, withStageProviderCapability(capability.GitHubPRRead))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	store, err := advisoryStore(root)
	if err != nil {
		pf(stderr, "error: open private advisory state: %v\n", err)
		return 1
	}
	ledger, err := openStageClaimLedger(instance.NewLayout(root))
	if err != nil {
		pf(stderr, "error: open claim ledger: %v\n", err)
		return 1
	}
	ctx, cancel := providerCommandContext()
	defer cancel()
	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{Repository: repo, SkipCheckState: true})
	if err != nil {
		return failProviderStage(stderr, "list open PRs", err, advisorySelectionFile)
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
	for _, pr := range prs {
		if pr.State != "open" {
			continue
		}
		key := advisoryKey(repo, reviewType, pr.Number)
		record, err := readAdvisoryDisposition(ctx, store, key)
		if err != nil {
			pf(stderr, "error: read private disposition for PR #%d: %v\n", pr.Number, err)
			return 1
		}
		if !record.SkippedAt.IsZero() || record.HeadSHA == pr.HeadSHA {
			continue
		}
		claim := advisoryClaimKey(repo, reviewType, pr.Number)
		claimed, _, err := ledger.ClaimScoped(ctx, claim, runID, workflow, 2*time.Hour)
		if err != nil {
			pf(stderr, "error: claim PR #%d: %v\n", pr.Number, err)
			return 1
		}
		if !claimed {
			continue
		}
		// A competing publisher may have committed a disposition between the
		// first read and this claim. The second read is mandatory.
		record, err = readAdvisoryDisposition(ctx, store, key)
		if err != nil {
			pf(stderr, "error: recheck private disposition for PR #%d: %v\n", pr.Number, err)
			return 1
		}
		if !record.SkippedAt.IsZero() || record.HeadSHA == pr.HeadSHA {
			_ = ledger.ReleaseScoped(ctx, claim, runID)
			continue
		}
		files, err := provider.PullRequestFiles(ctx, repo, pr.ID)
		if err != nil {
			return failProviderStage(stderr, "load PR diff", err, advisorySelectionFile)
		}
		selection := advisorySelection{Repository: advisoryScope(repo), ReviewType: reviewType,
			SelectedNumber: strconv.Itoa(pr.Number), SelectedHeadSHA: pr.HeadSHA,
			URL: pr.URL, Body: pr.Body, Draft: pr.Draft, Files: files}
		data, err := json.Marshal(selection)
		if err == nil {
			err = os.WriteFile(providerInput("resultFile", advisorySelectionFile), data, 0o644)
		}
		if err != nil {
			pf(stderr, "error: write selection: %v\n", err)
			return 1
		}
		pf(stdout, "selected PR #%d at %s for %s advisory review\n", pr.Number, pr.HeadSHA, reviewType)
		return 0
	}
	return writeAdvisoryResult(providerInput("resultFile", advisorySelectionFile),
		map[string]any{"noWork": true, "outcome": "every open PR is skipped, already reviewed at its current head, or claimed"}, stdout, stderr)
}

func strictAdvisoryReview(data []byte) (advisoryReview, error) {
	var review advisoryReview
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&review); err != nil {
		return review, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return review, fmt.Errorf("review artifact contains trailing data")
	}
	if review.Schema != advisorySchema || !advisoryTypePattern.MatchString(review.ReviewType) || review.Number <= 0 || len(review.HeadSHA) != 40 {
		return review, fmt.Errorf("review artifact has an invalid schema or identity")
	}
	switch review.Decision {
	case "skip":
		if review.Comment != "" {
			return review, fmt.Errorf("skip decision must have no comment")
		}
	case "interesting":
		if strings.TrimSpace(review.Comment) == "" || len(review.Comment) > 60000 {
			return review, fmt.Errorf("interesting decision requires a bounded substantive comment")
		}
	default:
		return review, fmt.Errorf("review decision must be skip or interesting")
	}
	return review, nil
}

func writeAdvisoryResult(path string, result map[string]any, stdout, stderr io.Writer) int {
	data, err := json.Marshal(result)
	if err == nil {
		err = os.WriteFile(path, data, 0o644)
	}
	if err != nil {
		pf(stderr, "error: write advisory result: %v\n", err)
		return 1
	}
	pf(stdout, "advisory review: %v\n", result["outcome"])
	return 0
}

func failAdvisoryArtifact(stderr io.Writer, resultFile, message string) int {
	pf(stderr, "error: %s\n", message)
	if err := writeProviderStageResult(resultFile, map[string]any{
		executor.OutputErrorCode:      "advisory_artifact_invalid",
		executor.OutputErrorMessage:   message,
		executor.OutputErrorRetryable: true,
	}); err != nil {
		pf(stderr, "error: write advisory failure result: %v\n", err)
	}
	return 1
}

func loadAdvisoryPublishArtifacts(root, resultFile, reviewType string, repo providers.RepositoryRef, stderr io.Writer) (advisorySelection, advisoryReview, bool) {
	selection, err := readDecompositionInput[advisorySelection](root, advisorySelectionFile, advisorySelectionFile,
		providerInput("selectionStage", "select-pr"), "/"+advisorySelectionFile)
	if err != nil {
		failAdvisoryArtifact(stderr, resultFile, fmt.Sprintf("read selector artifact: %v", err))
		return advisorySelection{}, advisoryReview{}, false
	}
	if selection.Repository != advisoryScope(repo) || selection.ReviewType != reviewType || selection.SelectedHeadSHA == "" {
		failAdvisoryArtifact(stderr, resultFile, "selector artifact identity differs from this review lane")
		return advisorySelection{}, advisoryReview{}, false
	}
	number, err := strconv.Atoi(selection.SelectedNumber)
	if err != nil || number <= 0 {
		failAdvisoryArtifact(stderr, resultFile, "selector artifact has invalid PR number")
		return advisorySelection{}, advisoryReview{}, false
	}
	raw, err := readDecompositionInput[json.RawMessage](root, advisoryReviewFile, advisoryReviewFile,
		providerInput("reviewerStage", "review"), "/"+advisoryReviewFile)
	if err != nil {
		failAdvisoryArtifact(stderr, resultFile, fmt.Sprintf("read reviewer artifact: %v", err))
		return advisorySelection{}, advisoryReview{}, false
	}
	review, err := strictAdvisoryReview(raw)
	if err != nil {
		failAdvisoryArtifact(stderr, resultFile, fmt.Sprintf("malformed reviewer artifact: %v", err))
		return advisorySelection{}, advisoryReview{}, false
	}
	if review.ReviewType != reviewType || review.Number != number || review.HeadSHA != selection.SelectedHeadSHA {
		failAdvisoryArtifact(stderr, resultFile, "reviewer artifact differs from selected PR/head/type")
		return advisorySelection{}, advisoryReview{}, false
	}
	return selection, review, true
}

func advisoryClaimHeld(ctx context.Context, root, runID string, repo providers.RepositoryRef, reviewType string, number int) (bool, error) {
	ledger, err := openStageClaimLedger(instance.NewLayout(root))
	if err != nil {
		return false, fmt.Errorf("open claim ledger: %w", err)
	}
	held, err := ledger.ForRunAll(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("verify advisory claim: %w", err)
	}
	claim := advisoryClaimKey(repo, reviewType, number)
	for _, entry := range held {
		if claimsclient.KeyForEntry(entry) == claim && entry.ExpiresAt.After(time.Now()) {
			return true, nil
		}
	}
	return false, nil
}

func persistAdvisorySkip(ctx context.Context, store stateclient.Store, key string) error {
	return store.Update(ctx, key, "advisory-pr.skip", func(value stateclient.Value) ([]byte, bool, error) {
		var next advisoryDisposition
		if len(value.Data) > 0 {
			if err := json.Unmarshal(value.Data, &next); err != nil {
				return nil, false, err
			}
		}
		if !next.SkippedAt.IsZero() {
			return nil, false, nil
		}
		next.SkippedAt = time.Now().UTC()
		data, err := json.Marshal(next)
		return data, true, err
	})
}

func runAdvisoryPRPublish(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("advisory-pr-publish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "advisory-pr-publish")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}
	reviewType, err := advisoryType()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	runID, _, err := providerRunContext()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if repo.Provider != providers.ProviderGitHub {
		pf(stderr, "error: advisory PR review currently requires GitHub\n")
		return 1
	}
	resultFile := providerInput("resultFile", advisoryResultFile)
	selection, review, valid := loadAdvisoryPublishArtifacts(root, resultFile, reviewType, repo, stderr)
	if !valid {
		return 1
	}
	ctx, cancel := providerCommandContext()
	defer cancel()
	owned, err := advisoryClaimHeld(ctx, root, runID, repo, reviewType, review.Number)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if !owned {
		pf(stderr, "error: this run no longer holds the selected advisory claim\n")
		return 1
	}
	store, err := advisoryStore(root)
	if err != nil {
		pf(stderr, "error: open private advisory state: %v\n", err)
		return 1
	}
	key := advisoryKey(repo, reviewType, review.Number)
	record, err := readAdvisoryDisposition(ctx, store, key)
	if err != nil {
		pf(stderr, "error: read private advisory state: %v\n", err)
		return 1
	}
	if !record.SkippedAt.IsZero() {
		return writeAdvisoryResult(resultFile, map[string]any{"outcome": "already-skipped"}, stdout, stderr)
	}
	provider, err := newProviderForStageAs[*providers.GitHubProvider](root, repo, false,
		withStageProviderCapability(capability.GitHubPRWrite))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	current, err := provider.GetPullRequest(ctx, repo, selection.SelectedNumber)
	if err != nil {
		return failProviderStage(stderr, "recheck PR", err, resultFile)
	}
	if current.State != "open" || current.HeadSHA != review.HeadSHA {
		return writeAdvisoryResult(resultFile, map[string]any{"outcome": "stale-or-closed"}, stdout, stderr)
	}
	if review.Decision == "skip" {
		if err := persistAdvisorySkip(ctx, store, key); err != nil {
			pf(stderr, "error: persist private skip: %v\n", err)
			return 1
		}
		return writeAdvisoryResult(resultFile, map[string]any{"outcome": "skipped-privately"}, stdout, stderr)
	}
	return publishInterestingAdvisory(ctx, provider, repo, selection, review, store, key, record,
		reviewType, resultFile, stdout, stderr)
}

func publishInterestingAdvisory(ctx context.Context, provider *providers.GitHubProvider, repo providers.RepositoryRef,
	selection advisorySelection, review advisoryReview, store stateclient.Store, key string, record advisoryDisposition,
	reviewType, resultFile string, stdout, stderr io.Writer,
) int {
	expectedAuthor := providerInput("expectedAuthor", "")
	if expectedAuthor == "" {
		pf(stderr, "error: expectedAuthor is required for advisory publication\n")
		return 1
	}
	actualAuthor, err := provider.AuthenticatedLogin(ctx)
	if err != nil {
		return failProviderStage(stderr, "resolve advisory publisher identity", err, resultFile)
	}
	if actualAuthor != expectedAuthor {
		pf(stderr, "error: advisory publisher identity %q differs from expectedAuthor %q\n", actualAuthor, expectedAuthor)
		return 1
	}
	sum := sha256.Sum256([]byte(review.Comment))
	digest := hex.EncodeToString(sum[:])
	marker := "<!-- goobers-advisory:" + reviewType + ":" + review.HeadSHA + ":" + digest + " -->"
	for _, receipt := range record.Receipts {
		if receipt == marker {
			return writeAdvisoryResult(resultFile, map[string]any{"outcome": "already-published"}, stdout, stderr)
		}
	}
	comments, err := provider.ListComments(ctx, repo, selection.SelectedNumber)
	if err != nil {
		return failProviderStage(stderr, "scan advisory comments", err, resultFile)
	}
	found := false
	for _, comment := range comments {
		if comment.Author == expectedAuthor && strings.Contains(comment.Body, marker) {
			found = true
			break
		}
	}
	if !found {
		// Recheck immediately before the only public mutation. A new head or a
		// closed PR remains retryable and creates no public trace.
		current, err := provider.GetPullRequest(ctx, repo, selection.SelectedNumber)
		if err != nil {
			return failProviderStage(stderr, "recheck PR before comment", err, resultFile)
		}
		if current.State != "open" || current.HeadSHA != review.HeadSHA {
			return writeAdvisoryResult(resultFile, map[string]any{"outcome": "stale-or-closed"}, stdout, stderr)
		}
		_, err = provider.CreateWorkItemComment(ctx, repo, selection.SelectedNumber, review.Comment+"\n\n"+marker)
		if err != nil {
			return failProviderStage(stderr, "post advisory comment", err, resultFile)
		}
	}
	if err := persistAdvisoryReceipt(ctx, store, key, marker, review.HeadSHA); err != nil {
		pf(stderr, "error: persist advisory receipt: %v\n", err)
		return 1
	}
	return writeAdvisoryResult(resultFile, map[string]any{"outcome": "published", "marker": marker}, stdout, stderr)
}

func persistAdvisoryReceipt(ctx context.Context, store stateclient.Store, key, marker, headSHA string) error {
	return store.Update(ctx, key, "advisory-pr.receipt", func(value stateclient.Value) ([]byte, bool, error) {
		var next advisoryDisposition
		if len(value.Data) > 0 {
			if err := json.Unmarshal(value.Data, &next); err != nil {
				return nil, false, err
			}
		}
		if !next.SkippedAt.IsZero() {
			return nil, false, fmt.Errorf("PR became privately skipped during publication")
		}
		for _, receipt := range next.Receipts {
			if receipt == marker {
				return nil, false, nil
			}
		}
		next.HeadSHA = headSHA
		next.Receipts = append(next.Receipts, marker)
		data, err := json.Marshal(next)
		return data, true, err
	})
}
