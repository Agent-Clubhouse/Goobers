package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	apiintegrity "github.com/goobers/goobers/api/integrity"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/decomposition"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

func TestBacklogQueryReadOnlyDoesNotMutateProviderOrScheduler(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Visible backlog item", "goobers:ready")

	providerCmdEnv(t, server, executor.CredentialEnvVar(string(capability.GitHubIssuesRead)), "read-only-run")
	schedulerDir := layoutFor(root).SchedulerDir()
	blockedPath := blockedRecordsPath(layoutFor(root))
	if err := os.WriteFile(blockedPath, []byte("{\"sentinel\":true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotDirectoryFiles(t, schedulerDir)

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "backlog-query", "--read-only", root)
	if code != 0 {
		t.Fatalf("backlog-query --read-only: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "7\tVisible backlog item") {
		t.Fatalf("stdout = %q, want visible backlog item", stdout)
	}
	if after := snapshotDirectoryFiles(t, schedulerDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("scheduler state changed: before = %#v, after = %#v", before, after)
	}
	if _, err := os.Stat(filepath.Join(workDir, mutationsSidecarFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mutation sidecar exists after read-only query: %v", err)
	}
}

func TestBacklogQueryReadOnlyReportsProviderFailure(t *testing.T) {
	root := initDemo(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	previousProvider := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		// The 503 exists to make the list fail, not to exercise the retry
		// ladder: spending the transient-retry budget keeps the assertion
		// identical while dropping 1+2+4+8 = 15s of real backoff sleep.
		return providers.NewGitHubProvider(token, append(opts, func(provider *providers.GitHubProvider) {
			provider.BaseURL = server.URL
		}, providers.WithMaxTransientRetries(0))...)
	}
	t.Cleanup(func() { newGitHubProvider = previousProvider })

	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesRead)), "read-token")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, _, stderr := runArgs(t, "backlog-query", "--read-only", root)
	if code != 1 || !strings.Contains(stderr, "list work items") {
		t.Fatalf("backlog-query --read-only provider failure: code = %d, stderr = %q", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read provider failure result: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal provider failure result: %v", err)
	}
	if message, _ := result[executor.OutputErrorMessage].(string); !strings.Contains(message, "list work items") {
		t.Fatalf("provider failure result = %#v, want list work items error", result)
	}
}

func snapshotDirectoryFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			files[relative+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[relative] = string(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

type failNthBacklogClaimLedger struct {
	backlogClaimLedger
	calls  int
	failAt int
}

func (l *failNthBacklogClaimLedger) Claim(itemID, runID, workflow string, leaseDuration time.Duration) (bool, string, error) {
	l.calls++
	if l.calls == l.failAt {
		return false, "", errors.New("injected claim failure")
	}
	return l.backlogClaimLedger.Claim(itemID, runID, workflow, leaseDuration)
}

func (l *failNthBacklogClaimLedger) ClaimScoped(key localscheduler.ClaimKey, runID, workflow string, leaseDuration time.Duration) (bool, string, error) {
	l.calls++
	if l.calls == l.failAt {
		return false, "", errors.New("injected claim failure")
	}
	return l.backlogClaimLedger.ClaimScoped(key, runID, workflow, leaseDuration)
}

// providerCmdEnv sets the GOOBERS_* env vars the runner would inject for a
// provider-chain stage process (#131/#132) and points newGitHubProvider at a
// fake server — the harness these CLI-level integration tests share.
func providerCmdEnv(t *testing.T, server *fakeGitHubServer, credCapability, runID string) {
	t.Helper()
	prev := newGitHubProvider
	newGitHubProvider = server.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })

	t.Setenv("GOOBERS_RUN_ID", runID)
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, server.owner)
	t.Setenv(executor.RepoNameEnvVar, server.repo)
	if credCapability != "" {
		t.Setenv(credCapability, "test-token")
		if credCapability == executor.CredentialEnvVar(string(capability.GitHubPRWrite)) {
			t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), "test-token")
		}
	}
}

// TestBacklogQueryClaimsEligibleItem is #131's core CLI-level acceptance:
// invoking `goobers backlog-query --claim` via the actual CLI entrypoint (not
// just a unit test on the underlying provider/ledger funcs) against a
// fake-provider e2e finds the one item carrying both the trust label
// (SEC-047) and the ready label, claims it in the local ledger (source of
// truth), mirrors the claim on the provider, and writes it to the declared
// result file.
func TestBacklogQueryClaimsEligibleItem(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers", "goobers:ready")
	server.addIssue(8, "Untrusted item", "goobers:ready") // missing trust label
	readyAt := time.Now().UTC().Add(-6 * time.Hour)
	server.setLabelEventTime(7, providers.LabelReady, true, readyAt)

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")

	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	if !strings.Contains(stdout, "claimed 7") {
		t.Fatalf("stdout = %q, want a mention of the claimed item", stdout)
	}

	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed-item.json: %v", err)
	}
	var claimed map[string]interface{}
	if err := json.Unmarshal(data, &claimed); err != nil {
		t.Fatalf("unmarshal claimed-item.json: %v", err)
	}
	if claimed["id"] != "7" {
		t.Fatalf("claimed item id = %v, want \"7\"", claimed["id"])
	}
	if claimed["integrity"] != string(apiintegrity.Maintainer) {
		t.Fatalf("claimed item integrity = %v, want maintainer", claimed["integrity"])
	}
	if claimed["readyAt"] != readyAt.Format(time.RFC3339Nano) {
		t.Fatalf("claimed readyAt = %v, want %s", claimed["readyAt"], readyAt.Format(time.RFC3339Nano))
	}

	// The claim ledger — the actual source of truth (#131) — durably holds
	// the claim, not just the provider-side marker.
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	entry, ok := ledger.Lookup("7")
	if !ok || entry.RunID != "run-1" {
		t.Fatalf("ledger entry for item 7 = %+v, ok=%v, want held by run-1", entry, ok)
	}
}

func TestBacklogQueryReleasesLedgerClaimAfterLosingProviderRace(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Raced item", "goobers:approved")
	server.addComment(7, "goobers-claim: run=other-instance-run\n\nClaimed by another instance.")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "losing-run")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 || !strings.Contains(stdout, "no work:") {
		t.Fatalf("claim race: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "claim race lost for item 7 to run other-instance-run") {
		t.Fatalf("stderr = %q, want detected-race warning", stderr)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := ledger.Lookup("7"); held {
		t.Fatalf("losing run retained ledger claim: %+v", entry)
	}
}

// TestBacklogQueryPlainScanReportsNoWorkForEmptyPumpTick locks in the #233
// gate for list/scan pumps: a plain backlog-query (no --claim) that declares a
// resultFile and finds nothing must emit ResultNoWork (noWork:true in the
// declared file) so the runner short-circuits to a clean PhaseCompleted before
// any downstream agentic stage runs. Without this, a scan-then-act workflow
// invoked its model-backed stage every empty tick only to rediscover there was
// nothing to act on — burning tokens on each poll.
func TestBacklogQueryPlainScanReportsNoWorkForEmptyPumpTick(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	// No approved issues seeded -> empty eligible set on this tick.
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "scan-run")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "scan.json")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", root)
	if code != 0 || !strings.Contains(stdout, "no work:") {
		t.Fatalf("empty pump scan: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "scan.json"))
	if err != nil {
		t.Fatalf("read scan.json: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal scan.json: %v", err)
	}
	if got["noWork"] != true {
		t.Fatalf("scan.json = %v, want noWork:true so the runner short-circuits before the agentic router", got)
	}
}

// TestBacklogQueryPlainScanWithWorkDoesNotGate guards the other side of the
// gate: a plain scan that finds eligible items must NOT report ResultNoWork, so
// the run proceeds to its downstream stage and actually does the work.
// Over-gating here would silently stall any scan-then-act workflow whenever the
// backlog was non-empty.
func TestBacklogQueryPlainScanWithWorkDoesNotGate(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(9, "Unlaned work", "goobers:approved")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "scan-run")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "scan.json")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", root)
	if code != 0 {
		t.Fatalf("scan with work: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "no work:") {
		t.Fatalf("scan with an eligible item must not gate as no-work: stdout = %q", stdout)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "scan.json"))
	if err != nil {
		t.Fatalf("read scan.json: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal scan.json: %v", err)
	}
	if got["noWork"] == true {
		t.Fatalf("scan.json = %v, want no noWork gate so the downstream router runs", got)
	}
}

func TestBacklogQueryRequiresParentPublishedRecordForDecompositionChild(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Tracking parent", providers.LabelApproved, providers.LabelTracking)
	server.addIssue(8, "Prepared child", "goobers", providers.LabelReady)
	server.addIssue(9, "Published child", "goobers", providers.LabelReady)
	const digest = "sha256:batch"
	server.mu.Lock()
	server.issues[8].body = decomposition.ChildBatchMarker("7", digest, "prepared")
	server.issues[9].body = decomposition.ChildBatchMarker("7", digest, "published")
	server.mu.Unlock()
	server.addComment(7, decomposition.PublishedBatchRecord("7", digest, []string{"9"}))

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-barrier")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", providers.LabelReady)
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	data, err := os.ReadFile("claimed-item.json")
	if err != nil {
		t.Fatal(err)
	}
	var claimed providers.WorkItem
	if err := json.Unmarshal(data, &claimed); err != nil {
		t.Fatal(err)
	}
	if claimed.ID != "9" {
		t.Fatalf("claimed %s, want published child 9; prepared child 8 must remain ineligible", claimed.ID)
	}
}

type decompositionBarrierProvider struct {
	parent      providers.WorkItem
	comments    []providers.Comment
	commentsErr error
}

func (p decompositionBarrierProvider) ListWorkItems(context.Context, providers.ListWorkItemsRequest) ([]providers.WorkItem, error) {
	return nil, nil
}
func (p decompositionBarrierProvider) GetWorkItem(context.Context, providers.RepositoryRef, string) (providers.WorkItem, error) {
	return p.parent, nil
}
func (p decompositionBarrierProvider) ListComments(context.Context, providers.RepositoryRef, string) ([]providers.Comment, error) {
	return p.comments, p.commentsErr
}
func (p decompositionBarrierProvider) CreateWorkItem(context.Context, providers.CreateWorkItemRequest) (providers.WorkItem, error) {
	return providers.WorkItem{}, nil
}
func (p decompositionBarrierProvider) UpdateWorkItem(context.Context, providers.UpdateWorkItemRequest) (providers.WorkItem, error) {
	return providers.WorkItem{}, nil
}
func (p decompositionBarrierProvider) UpdateWorkItemStatus(context.Context, providers.UpdateWorkItemStatusRequest) (providers.WorkItem, error) {
	return providers.WorkItem{}, nil
}
func (p decompositionBarrierProvider) ClaimWorkItem(context.Context, providers.ClaimWorkItemRequest) (providers.ClaimResult, error) {
	return providers.ClaimResult{}, nil
}

func TestDecompositionEligibilityBarrierFailsClosedOnProviderError(t *testing.T) {
	const digest = "sha256:batch"
	providerErr := errors.New("comments unavailable")
	_, err := filterDecompositionEligibility(
		context.Background(),
		decompositionBarrierProvider{parent: providers.WorkItem{ID: "7"}, commentsErr: providerErr},
		providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		[]providers.WorkItem{{ID: "8", Body: decomposition.ChildBatchMarker("7", digest, "child")}},
	)
	if !errors.Is(err, providerErr) {
		t.Fatalf("error = %v, want provider failure", err)
	}
}

func TestDecompositionEligibilityBarrierRejectsConflictingPublishedRecord(t *testing.T) {
	const digest = "sha256:batch"
	items, err := filterDecompositionEligibility(
		context.Background(),
		decompositionBarrierProvider{
			parent: providers.WorkItem{ID: "7"},
			comments: []providers.Comment{
				{Body: decomposition.PublishedBatchRecord("7", digest, []string{"8"})},
				{Body: decomposition.PublishedBatchRecord("7", "sha256:other", []string{"9"})},
			},
		},
		providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
		[]providers.WorkItem{{ID: "8", Body: decomposition.ChildBatchMarker("7", digest, "child")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("eligible items = %v, want conflicting batch to fail closed", items)
	}
}

func TestDecompositionEligibilityBarrierRejectsMalformedChildMarkers(t *testing.T) {
	const digest = "sha256:batch"
	provider := decompositionBarrierProvider{
		parent:   providers.WorkItem{ID: "7"},
		comments: []providers.Comment{{Body: decomposition.PublishedBatchRecord("7", digest, []string{"8"})}},
	}
	for name, marker := range map[string]string{
		"extra field":      decomposition.ChildBatchMarker("7", digest, "child") + " extra=value",
		"reordered fields": decomposition.ChildBatchMarkerPrefix + " v1 digest=" + digest + " parent=7 key=child",
		"duplicate field":  decomposition.ChildBatchMarkerPrefix + " v1 parent=7 digest=" + digest + " parent=7",
	} {
		t.Run(name, func(t *testing.T) {
			items, err := filterDecompositionEligibility(
				context.Background(),
				provider,
				providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "app"},
				[]providers.WorkItem{{ID: "8", Body: marker}},
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 0 {
				t.Fatalf("eligible items = %v, want malformed marker to fail closed", items)
			}
		})
	}
}

func TestBacklogQuerySkipsMalformedReadyItemAndClaimsNext(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Malformed oldest item", "goobers:approved", "goobers:ready")
	server.addIssue(8, "Healthy later item", "goobers:approved", "goobers:ready")
	server.mu.Lock()
	for i, event := range server.issueEvents {
		if event.number == 7 && event.label == providers.LabelReady {
			server.issueEvents = append(server.issueEvents[:i], server.issueEvents[i+1:]...)
			break
		}
	}
	server.mu.Unlock()

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 || !strings.Contains(stdout, "claimed 8") {
		t.Fatalf("claim after malformed item: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "skipping malformed eligible item 7") {
		t.Fatalf("stderr = %q, want malformed-item warning", stderr)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := ledger.Lookup("7"); held {
		t.Fatalf("malformed item retained claim: %+v", entry)
	}
	if entry, held := ledger.Lookup("8"); !held || entry.RunID != "run-1" {
		t.Fatalf("healthy item claim = %+v, held = %v; want run-1", entry, held)
	}
}

func TestBacklogQueryPartialBatchFailureReleasesEarlierClaims(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "First item", "goobers:approved")
	server.addIssue(8, "Second item", "goobers:approved")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "batch-run")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "2")
	t.Chdir(t.TempDir())

	originalOpen := openBacklogClaimLedger
	openBacklogClaimLedger = func(path string, opts ...localscheduler.LedgerOption) (backlogClaimLedger, error) {
		ledger, err := localscheduler.OpenClaimLedger(path, opts...)
		if err != nil {
			return nil, err
		}
		return &failNthBacklogClaimLedger{backlogClaimLedger: ledger, failAt: 2}, nil
	}
	t.Cleanup(func() { openBacklogClaimLedger = originalOpen })

	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 || !strings.Contains(stderr, "injected claim failure") {
		t.Fatalf("partial batch failure: code = %d, stderr = %q", code, stderr)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	if entries := ledger.ForRunAll("batch-run"); len(entries) != 0 {
		t.Fatalf("partial batch retained claims: %+v", entries)
	}
}

func TestBacklogQueryBatchFailurePreservesPreexistingClaim(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Previously claimed item", "goobers:approved")
	server.addIssue(8, "New item", "goobers:approved")
	server.addIssue(9, "Failing item", "goobers:approved")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "batch-run")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "3")
	t.Setenv("GOOBERS_GAGGLE", "goobers")
	t.Chdir(t.TempDir())

	ledgerPath := filepath.Join(root, "scheduler", "claims.json")
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	key := localscheduler.ClaimKey{Gaggle: "goobers", Provider: string(providers.ProviderGitHub), ExternalID: "7"}
	if ok, _, err := ledger.ClaimScoped(key, "batch-run", "implementation", DefaultClaimLease); err != nil || !ok {
		t.Fatalf("seed preexisting claim: ok = %v, err = %v", ok, err)
	}

	originalOpen := openBacklogClaimLedger
	openBacklogClaimLedger = func(path string, opts ...localscheduler.LedgerOption) (backlogClaimLedger, error) {
		ledger, err := localscheduler.OpenClaimLedger(path, opts...)
		if err != nil {
			return nil, err
		}
		return &failNthBacklogClaimLedger{backlogClaimLedger: ledger, failAt: 3}, nil
	}
	t.Cleanup(func() { openBacklogClaimLedger = originalOpen })

	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 || !strings.Contains(stderr, "injected claim failure") {
		t.Fatalf("batch failure: code = %d, stderr = %q", code, stderr)
	}
	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	entries := reopened.ForRunAll("batch-run")
	if len(entries) != 1 || entries[0].ItemID != "7" {
		t.Fatalf("claims after rollback = %+v, want only preexisting item 7", entries)
	}
}

func TestBacklogQueryResultFailureDoesNotPublishProviderMarker(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Claim candidate", "goobers:approved")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", filepath.Join(t.TempDir(), "missing", "claimed-item.json"))
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 || !strings.Contains(stderr, "write") {
		t.Fatalf("result failure: code = %d, stderr = %q", code, stderr)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	if entry, held := ledger.Lookup("7"); held {
		t.Fatalf("result failure retained ledger claim: %+v", entry)
	}
	server.mu.Lock()
	labels := append([]string(nil), server.issues[7].labels...)
	server.mu.Unlock()
	if hasAnyLabel(labels, []string{"goobers:claimed"}) {
		t.Fatalf("result failure published provider marker: labels = %v", labels)
	}
}

func TestBacklogQueryLabelLists(t *testing.T) {
	tests := []struct {
		name          string
		requireLabels string
		excludeLabels string
		issueLabels   [][]string
		wantIDs       string
	}{
		{
			name:          "require single",
			requireLabels: "a",
			issueLabels:   [][]string{{"trusted", "a"}, {"trusted", "b"}},
			wantIDs:       "7",
		},
		{
			name:          "require multiple",
			requireLabels: "a,b",
			issueLabels:   [][]string{{"trusted", "a", "b"}, {"trusted", "a"}},
			wantIDs:       "7",
		},
		{
			name:          "require spaced",
			requireLabels: "a, b",
			issueLabels:   [][]string{{"trusted", "a", "b"}, {"trusted", "a"}},
			wantIDs:       "7",
		},
		{
			name:        "require empty",
			issueLabels: [][]string{{"trusted"}},
			wantIDs:     "7",
		},
		{
			name:          "exclude single",
			excludeLabels: "a",
			issueLabels:   [][]string{{"trusted", "a"}, {"trusted", "b"}},
			wantIDs:       "8",
		},
		{
			name:          "exclude multiple",
			excludeLabels: "a,b",
			issueLabels:   [][]string{{"trusted", "a"}, {"trusted", "b"}, {"trusted", "c"}},
			wantIDs:       "9",
		},
		{
			name:          "exclude spaced",
			excludeLabels: "a, b",
			issueLabels:   [][]string{{"trusted", "a"}, {"trusted", "b"}, {"trusted", "c"}},
			wantIDs:       "9",
		},
		{
			name:        "exclude empty",
			issueLabels: [][]string{{"trusted"}},
			wantIDs:     "7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			for i, labels := range tt.issueLabels {
				server.addIssue(7+i, "Candidate", labels...)
			}

			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
			t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "trusted")
			t.Setenv("GOOBERS_INPUT_REQUIRELABELS", tt.requireLabels)
			t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", tt.excludeLabels)
			t.Chdir(t.TempDir())

			code, stdout, stderr := runArgs(t, "backlog-query", root)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			var gotIDs []string
			for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
				if id, _, ok := strings.Cut(line, "\t"); ok {
					gotIDs = append(gotIDs, id)
				}
			}
			if got := strings.Join(gotIDs, ","); got != tt.wantIDs {
				t.Fatalf("eligible IDs = %q, want %q; stdout = %q", got, tt.wantIDs, stdout)
			}
		})
	}
}

// TestBacklogQueryRespectAssignee is #1820's (COORD-2) core acceptance:
// opted-out is byte-identical to today regardless of assignee, and each
// opted-in mode — fixed identity, null/unassigned-only, and the exclusion
// case that must not be conflated with null mode (assigned to someone else,
// not merely unassigned) — enforces the right eligibility.
func TestBacklogQueryRespectAssignee(t *testing.T) {
	tests := []struct {
		name            string
		respectAssignee string
		assignedTo      string
		issueAssignees  []string // index 0 -> issue 7, index 1 -> issue 8, index 2 -> issue 9
		wantIDs         string
	}{
		{
			name:            "opted out ignores assignee entirely",
			respectAssignee: "",
			assignedTo:      "mason",
			issueAssignees:  []string{"mason", "someone-else", ""},
			wantIDs:         "7,8,9",
		},
		{
			name:            "fixed identity only matches that login",
			respectAssignee: "true",
			assignedTo:      "mason",
			issueAssignees:  []string{"mason", "someone-else", ""},
			wantIDs:         "7",
		},
		{
			name:            "null mode matches only unassigned items",
			respectAssignee: "true",
			assignedTo:      "",
			issueAssignees:  []string{"mason", "someone-else", ""},
			wantIDs:         "9",
		},
		{
			name:            "assigned to someone else is excluded, not conflated with null mode",
			respectAssignee: "true",
			assignedTo:      "mason",
			issueAssignees:  []string{"someone-else"},
			wantIDs:         "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			for i, assignee := range tt.issueAssignees {
				number := 7 + i
				server.addIssue(number, "Candidate", "trusted")
				if assignee != "" {
					server.issues[number].assignee = assignee
				}
			}

			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
			t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "trusted")
			t.Setenv("GOOBERS_INPUT_RESPECTASSIGNEE", tt.respectAssignee)
			t.Setenv("GOOBERS_INPUT_ASSIGNEDTO", tt.assignedTo)
			t.Chdir(t.TempDir())

			code, stdout, stderr := runArgs(t, "backlog-query", root)
			if code != 0 {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			var gotIDs []string
			for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
				if id, _, ok := strings.Cut(line, "\t"); ok {
					gotIDs = append(gotIDs, id)
				}
			}
			if got := strings.Join(gotIDs, ","); got != tt.wantIDs {
				t.Fatalf("eligible IDs = %q, want %q; stdout = %q", got, tt.wantIDs, stdout)
			}
		})
	}
}

// TestBacklogQueryRespectAssigneeClaimsOnlyMatchingIdentity verifies the
// filter also governs --claim (the eligibility loop it shares with list),
// and that a matching item is actually claimable end-to-end, not just
// listed.
func TestBacklogQueryRespectAssigneeClaimsOnlyMatchingIdentity(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Assigned to bot", "trusted")
	server.issues[7].assignee = "gaggle-bot"
	server.addIssue(8, "Assigned to someone else", "trusted")
	server.issues[8].assignee = "someone-else"

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "trusted")
	t.Setenv("GOOBERS_INPUT_RESPECTASSIGNEE", "true")
	t.Setenv("GOOBERS_INPUT_ASSIGNEDTO", "gaggle-bot")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("backlog-query: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "claimed 7") {
		t.Fatalf("stdout = %q, want a mention of claiming item 7 (assigned to gaggle-bot)", stdout)
	}

	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(root, "scheduler", "claims.json"))
	if err != nil {
		t.Fatalf("open claim ledger: %v", err)
	}
	if _, ok := ledger.Lookup("8"); ok {
		t.Fatalf("item 8 (assigned to someone-else) must not be claimed under assignedTo=gaggle-bot")
	}
}

func TestBacklogQueryAppliesExactLabelPredicate(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Small runner item", "trusted", "area:runner", "size:s")
	server.addIssue(8, "Windows medium item", "trusted", "area:runner", "size:m", "platform:windows")
	server.addIssue(9, "Large runner item", "trusted", "area:runner", "size:l")
	server.addIssue(10, "Small docs item", "trusted", "area:docs", "size:s")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "trusted")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "area:runner")
	t.Setenv("GOOBERS_INPUT_LABELPREDICATE", `("size:s" in labels || "size:m" in labels) && !("platform:windows" in labels)`)
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "7\tSmall runner item") || strings.Contains(stdout, "8\t") ||
		strings.Contains(stdout, "9\t") || strings.Contains(stdout, "10\t") {
		t.Fatalf("stdout = %q, want only issue 7", stdout)
	}
}

func TestBacklogQueryRejectsInvalidLabelPredicate(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_LABELPREDICATE", `labels.size() > 0`)
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", root)
	if code != 1 || !strings.Contains(stderr, "invalid labelPredicate") {
		t.Fatalf("code = %d, stderr = %q, want fail-closed predicate validation", code, stderr)
	}
}

func TestBacklogQueryAppliesNativeFieldPredicate(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Old item", "trusted")
	server.addIssue(8, "First selected item", "trusted")
	server.addIssue(9, "Second selected item", "trusted")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "trusted")
	t.Setenv("GOOBERS_INPUT_FIELDPREDICATE", `fields["number"] >= 8`)
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "7\t") || !strings.Contains(stdout, "8\tFirst selected item") ||
		!strings.Contains(stdout, "9\tSecond selected item") {
		t.Fatalf("stdout = %q, want only issues 8 and 9", stdout)
	}
}

func TestBacklogQueryOrdersByNativeField(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Old item", "trusted")
	server.addIssue(8, "Middle item", "trusted")
	server.addIssue(9, "New item", "trusted")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "trusted")
	t.Setenv("GOOBERS_INPUT_FIELDORDER", "number:desc")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if lines := strings.Split(strings.TrimSpace(stdout), "\n"); len(lines) != 3 ||
		!strings.HasPrefix(lines[0], "9\t") || !strings.HasPrefix(lines[1], "8\t") ||
		!strings.HasPrefix(lines[2], "7\t") {
		t.Fatalf("stdout = %q, want descending issue-number order", stdout)
	}
}

func TestBacklogQueryFieldOrderScansBeyondFIFOWindow(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for i := 1; i <= backlogScanCeiling+1; i++ {
		server.addIssue(i, "Candidate", "trusted")
	}

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-ordered")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "trusted")
	t.Setenv("GOOBERS_INPUT_FIELDORDER", "number:desc")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	wantID := strconv.Itoa(backlogScanCeiling + 1)
	if !strings.Contains(stdout, "claimed "+wantID+": Candidate") {
		t.Fatalf("stdout = %q, want highest issue beyond the FIFO scan ceiling", stdout)
	}
	gotPages := server.issueListPageSizeHistory()
	wantPages := (backlogScanCeiling + 1 + backlogScanPageSize - 1) / backlogScanPageSize
	if len(gotPages) != wantPages {
		t.Fatalf("issue page sizes = %v, want %d exhaustive pages", gotPages, wantPages)
	}
	for _, pageSize := range gotPages {
		if pageSize != backlogScanPageSize {
			t.Fatalf("issue page sizes = %v, want full-size pages for an exhaustive scan", gotPages)
		}
	}
}

func TestBacklogQueryUnavailableNativeFieldFails(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Candidate", "trusted")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "trusted")
	t.Setenv("GOOBERS_INPUT_FIELDPREDICATE", `fields["project.priority"] == 1`)
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", root)
	if code != 1 || !strings.Contains(stderr, `field "project.priority" is unavailable`) {
		t.Fatalf("code = %d, stderr = %q, want unavailable-field error", code, stderr)
	}
}

func TestBacklogQueryUnavailableOrderFieldFails(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Candidate", "trusted")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "trusted")
	t.Setenv("GOOBERS_INPUT_FIELDORDER", "milestone.number:asc")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", root)
	if code != 1 || !strings.Contains(stderr, `field "milestone.number" is unavailable`) {
		t.Fatalf("code = %d, stderr = %q, want unavailable-field order error", code, stderr)
	}
}

func TestBacklogQueryCurationExcludesReadyItem(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Already curated", "goobers:approved", "goobers:ready")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Setenv("GOOBERS_INPUT_CURATION", "true")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", "goobers:ready,goobers:needs-human")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "20")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want ready-labeled item skipped as no work", stdout)
	}
	assertNoWorkResultFile(t, workDir)
	if _, err := os.Stat(filepath.Join(root, "scheduler", "claims.json")); err == nil {
		t.Fatal("curation should not claim an already-ready item")
	}
}

// TestBacklogQueryUnlabeledItemNeverClaimed proves SEC-047 eligibility is
// enforced in code, not just documented: an item missing the trust label is
// never claimed even though it's otherwise ready. Also issue #233's core
// acceptance: an empty eligible set is a clean no-work exit 0, not a
// business-error exit 1 — every idle tick must not poison telemetry as a
// false run failure.
func TestBacklogQueryUnlabeledItemNeverClaimed(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(9, "Untrusted item", "goobers:ready") // no trust label

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (empty backlog is no-work, not a failure), stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want a clear no-work message", stdout)
	}
	assertNoWorkResultFile(t, workDir)
}

// assertNoWorkResultFile confirms the default resultFile carries the
// structured no-work outcome and its provenance, not a generic provider
// failure envelope.
func assertNoWorkResultFile(t *testing.T, workDir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workDir, "claimed-item.json"))
	if err != nil {
		t.Fatalf("read claimed-item.json: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal claimed-item.json: %v", err)
	}
	if got["noWork"] != true {
		t.Fatalf("claimed-item.json = %v, want noWork:true", got)
	}
	if got["claimed"] != false {
		t.Fatalf("claimed-item.json = %v, want claimed:false", got)
	}
	if got["integrity"] != string(apiintegrity.Unapproved) {
		t.Fatalf("claimed-item.json = %v, want unapproved integrity", got)
	}
	for _, key := range []string{
		executor.OutputErrorCode,
		executor.OutputErrorMessage,
		executor.OutputErrorRetryable,
	} {
		if _, ok := got[key]; ok {
			t.Fatalf("claimed-item.json = %v, business no-work must not use the generic provider failure envelope", got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("claimed-item.json = %v, want only claimed, noWork, and integrity", got)
	}
}

// TestBacklogQuerySecondRunLosesTheClaimRace is #131's "two concurrent runs,
// one claim wins" acceptance criterion, driven sequentially through the CLI
// (env vars are process-global, so genuinely concurrent goroutines can't
// each carry a distinct GOOBERS_RUN_ID within one test binary) — the
// exclusivity property under test is the claim ledger's own atomicity
// (already proven under real concurrency at the ledger level by
// internal/localscheduler's TestClaimConcurrentRace), not raw goroutine
// timing. Also issue #233: the loser's outcome is now a clean no-work exit
// 0, not a business-error exit 1 — losing a claim race is exactly as
// routine as an empty backlog, not a failure.
func TestBacklogQuerySecondRunLosesTheClaimRace(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Chdir(t.TempDir())
	if code, _, stderr := runArgs(t, "backlog-query", "--claim", root); code != 0 {
		t.Fatalf("first claim: code = %d, stderr = %q", code, stderr)
	}

	t.Setenv("GOOBERS_RUN_ID", "run-2")
	workDir := t.TempDir()
	t.Chdir(workDir)
	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("second claim: code = %d, want 0 (losing the race is no-work, not a failure), stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want a clear no-work message", stdout)
	}
	assertNoWorkResultFile(t, workDir)
}

// TestBacklogQueryExcludesIssueWithOpenPR is #414's core acceptance: an item
// that's otherwise eligible by label (goobers:approved + goobers:ready, no
// exclude label present — simulating a missed or since-removed in-review/
// claimed label write) must still be excluded once an open goober-authored
// PR references it via a closing keyword, the same convention `goobers
// open-pr` writes and `goobers post-merge` parses at merge time. Requires
// the query-backlog stage to actually declare github:pr:write — the
// backstop is opt-in per stage (see the next test for the ungranted case).
func TestBacklogQueryExcludesIssueWithOpenPR(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")
	server.addOpenPR(101, "goobers/implementation/prior-run", "main", "sha1", "sha2", false, nil, nil)
	server.setPRBody(101, "Implements the fix.\n\nFixes #7")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (no eligible item is no-work, not a failure), stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want a clear no-work message — issue 7 should be excluded by the open-PR backstop", stdout)
	}
	assertNoWorkResultFile(t, workDir)
}

// TestBacklogQueryExcludesIssueWithImplementsOnlyPR is #980: the open-PR
// backstop must exclude an issue whose only open PR references it via the
// non-closing "Implements #N" convention (a structured body whose "Fixes #N"
// footer was overridden or absent), not just one carrying a closing keyword.
// This is the gap that let issue #774 be implemented twice (#966/#969).
func TestBacklogQueryExcludesIssueWithImplementsOnlyPR(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Convert the logs", "goobers:approved", "goobers:ready")
	server.addOpenPR(101, "goobers/implementation/prior-run", "main", "sha1", "sha2", false, nil, nil)
	server.setPRBody(101, "## Summary\n\nImplements #7: **Convert the logs**.")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_CRED_GITHUB_PR_WRITE", "test-token")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	workDir := t.TempDir()
	t.Chdir(workDir)

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "no work") {
		t.Fatalf("stdout = %q, want no-work — issue 7 should be excluded by the widened open-PR backstop", stdout)
	}
	assertNoWorkResultFile(t, workDir)
}

// TestBacklogQueryOpenPRBackstopSkippedWithoutCapability proves the backstop
// is opt-in, not a hard requirement: a stage that never declared
// github:pr:write gets exactly the pre-#414 label-only behavior (the item is
// still eligible) rather than backlog-query failing closed on a capability
// it was never granted — the label check above remains the primary
// eligibility gate; this backstop only adds to it when available.
func TestBacklogQueryOpenPRBackstopSkippedWithoutCapability(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")
	server.addOpenPR(101, "goobers/implementation/prior-run", "main", "sha1", "sha2", false, nil, nil)
	server.setPRBody(101, "Fixes #7")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	// Deliberately no GOOBERS_CRED_GITHUB_PR_WRITE.
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "claimed 7") {
		t.Fatalf("stdout = %q, want item 7 claimed (backstop not active without the capability)", stdout)
	}
}

// TestBacklogQueryListsWithoutClaiming proves the no-flag form is read-only:
// it reports eligible items but does not touch the claim ledger.
func TestBacklogQueryListsWithoutClaiming(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "7") {
		t.Fatalf("stdout = %q, want the eligible item listed", stdout)
	}

	if _, err := os.Stat(filepath.Join(root, "scheduler", "claims.json")); err == nil {
		t.Fatal("list-only mode should not have touched the claim ledger")
	}
}

// TestBacklogQueryClaimFailsClosedWithoutTrustLabel proves SEC-047 fails
// CLOSED, not open: --claim with no declared trustLabel must refuse to
// claim rather than silently skip the trust check and claim anything
// eligible by requireLabels alone (an item with no trust label at all must
// still never be claimed).
func TestBacklogQueryClaimFailsClosedWithoutTrustLabel(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:ready") // no trust label at all

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	// Deliberately no GOOBERS_INPUT_TRUSTLABEL.
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (fail closed on missing trustLabel), stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "trustLabel is required") {
		t.Fatalf("stderr = %q, want a clear missing-trustLabel message", stderr)
	}

	// Confirm nothing was claimed.
	if _, err := os.Stat(filepath.Join(root, "scheduler", "claims.json")); err == nil {
		t.Fatal("fail-closed rejection should not have touched the claim ledger")
	}
}

// TestBacklogQueryListWithoutTrustLabelStillWorks proves the fail-closed
// SEC-047 guard is scoped to --claim (the mutating, consequential action) —
// a plain read-only list doesn't require trustLabel to be declared.
func TestBacklogQueryListWithoutTrustLabelStillWorks(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:ready")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "7") {
		t.Fatalf("stdout = %q, want the item listed", stdout)
	}
}

// TestBacklogQueryMissingRunIDFailsClosed proves backlog-query refuses to
// claim without a real run identity (GOOBERS_RUN_ID) rather than proceeding
// under an empty/synthetic one that could collide with a real run's claims.
func TestBacklogQueryMissingRunIDFailsClosed(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	prev := newGitHubProvider
	newGitHubProvider = server.newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = prev })
	t.Setenv("GOOBERS_CRED_GITHUB_ISSUES_WRITE", "test-token")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	// #321: a live local-ci `go test ./...` inherits the run's real
	// GOOBERS_RUN_ID/GOOBERS_WORKFLOW from internal/executor.buildStageEnv, which
	// silently defeated this fail-closed test on every run. Simulate that
	// parent-process leak, then clear it — so the test genuinely exercises the
	// missing-run-context path AND regression-guards the fix under normal CI
	// (which has no ambient run context of its own to reproduce the leak).
	t.Setenv("GOOBERS_RUN_ID", "ambient-parent-leak")
	t.Setenv("GOOBERS_WORKFLOW", "ambient-parent-leak")
	unsetRunContext(t)
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (fail closed on missing GOOBERS_RUN_ID), stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "GOOBERS_RUN_ID") {
		t.Fatalf("stderr = %q, want a clear missing-run-id message", stderr)
	}
}

// TestBacklogQueryRejectsNonPositiveLeaseDuration is issue #235's edge 1,
// exercised at the CLI level: a workflow's leaseDuration input of "0s" (or
// any non-positive duration) must fail closed with an actionable message —
// this is the same class of authoring mistake trustLabel's own fail-closed
// check guards against, and it must be caught here, not just deep inside
// ClaimLedger.Claim, so the error names the actual bad input.
func TestBacklogQueryRejectsNonPositiveLeaseDuration(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:ready")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "run-1")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_LEASEDURATION", "0s")
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (fail closed on non-positive leaseDuration), stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "leaseDuration") || !strings.Contains(stderr, "must be positive") {
		t.Fatalf("stderr = %q, want an actionable leaseDuration message", stderr)
	}

	// Confirm nothing was claimed — the ledger file must not even exist.
	if _, err := os.Stat(filepath.Join(root, "scheduler", "claims.json")); err == nil {
		t.Fatal("fail-closed rejection should not have touched the claim ledger")
	}
}

// TestBacklogQueryReleaseUnblocksAFollowUpClaim covers #234 and #1003 together:
// a real curation claim adds both the authoritative ledger lease and its
// provider mirror; release removes both while preserving curation's ready label,
// and an implementation run can immediately claim the item.
func TestBacklogQueryReleaseUnblocksAFollowUpClaim(t *testing.T) {
	root := initDemo(t)
	schedulerDir := filepath.Join(root, "scheduler")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Setenv("GOOBERS_INPUT_CURATION", "true")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", "goobers:ready,goobers:needs-human")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "20")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "claimed-items.json")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("claim: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	server.mu.Lock()
	server.issues[7].labels = append(server.issues[7].labels, "goobers:ready")
	server.appendLabelEventLocked(7, providers.LabelReady, true, time.Now().UTC())
	claimedLabels := append([]string(nil), server.issues[7].labels...)
	server.mu.Unlock()
	if !hasAnyLabel(claimedLabels, []string{"goobers:claimed"}) {
		t.Fatalf("labels after claim = %v, want goobers:claimed", claimedLabels)
	}

	code, stdout, stderr = runArgs(t, "backlog-query", "--release", root)
	if code != 0 {
		t.Fatalf("release: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "released 7") {
		t.Fatalf("stdout = %q, want a mention of the released item", stdout)
	}
	server.mu.Lock()
	releasedLabels := append([]string(nil), server.issues[7].labels...)
	server.mu.Unlock()
	if hasAnyLabel(releasedLabels, []string{"goobers:claimed"}) {
		t.Fatalf("labels after release = %v, want goobers:claimed removed", releasedLabels)
	}
	if !hasAnyLabel(releasedLabels, []string{"goobers:ready"}) {
		t.Fatalf("labels after release = %v, want curator's goobers:ready preserved", releasedLabels)
	}

	// No residual lease: ForRun finds nothing for the curation run.
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, held := reopened.ForRun("curation-run"); held {
		t.Fatal("curation run should hold no claim after release")
	}

	// Exercise the real follow-up claim so the provider breadcrumb protocol is
	// covered as well as the ledger. The implementation selector requires the
	// ready label that curation added above.
	t.Setenv("GOOBERS_RUN_ID", "impl-run")
	t.Setenv("GOOBERS_WORKFLOW", "implementation")
	t.Setenv("GOOBERS_INPUT_REQUIRELABELS", "goobers:ready")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", "goobers/status:in-review")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "1")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "claimed-item.json")
	t.Chdir(t.TempDir())
	code, stdout, stderr = runArgs(t, "backlog-query", "--claim", root)
	if code != 0 || !strings.Contains(stdout, "claimed 7") {
		t.Fatalf("implementation claim: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	reopened, err = localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	entry, held := reopened.Lookup("7")
	if !held || entry.RunID != "impl-run" {
		t.Fatalf("implementation ledger claim = %+v, held=%v; want impl-run", entry, held)
	}
	server.mu.Lock()
	implementationLabels := append([]string(nil), server.issues[7].labels...)
	server.mu.Unlock()
	if !hasAnyLabel(implementationLabels, []string{"goobers:claimed"}) {
		t.Fatalf("labels after implementation claim = %v, want goobers:claimed restored", implementationLabels)
	}
}

func TestBacklogQueryReleaseReconcilesHistoricalProviderClaim(t *testing.T) {
	root := initDemo(t)
	schedulerDir := filepath.Join(root, "scheduler")
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(7, "Fix the bug", "goobers:approved", "goobers:claimed")
	server.addComment(7, "goobers-claim: run=historical-run\n\nClaimed by an earlier Goobers version.")

	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Setenv("GOOBERS_INPUT_CURATION", "true")
	t.Setenv("GOOBERS_INPUT_TRUSTLABEL", "goobers:approved")
	t.Setenv("GOOBERS_INPUT_EXCLUDELABELS", "goobers:ready,goobers:needs-human")
	t.Setenv("GOOBERS_INPUT_MAXITEMS", "20")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", "claimed-items.json")
	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--claim", root)
	if code != 0 {
		t.Fatalf("claim over historical marker: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	entry, held := ledger.ForRun("curation-run")
	if !held || entry.ItemID != "7" {
		t.Fatalf("curation ledger claim = %+v, held=%v; want item 7", entry, held)
	}

	code, stdout, stderr = runArgs(t, "backlog-query", "--release", root)
	if code != 0 {
		t.Fatalf("release historical marker: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	reopened, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, held := reopened.ForRun("curation-run"); held {
		t.Fatal("curation run should hold no claim after reconciling release")
	}
	server.mu.Lock()
	labels := append([]string(nil), server.issues[7].labels...)
	server.mu.Unlock()
	if hasAnyLabel(labels, []string{"goobers:claimed"}) {
		t.Fatalf("labels after historical release = %v, want goobers:claimed removed", labels)
	}
}

func TestBacklogQueryReleaseReleasesAllClaims(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	for _, itemID := range []int{7, 8, 9, 10} {
		server.addIssue(itemID, "Claimed item", "goobers:claimed")
	}
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")

	ledgerPath := filepath.Join(root, "scheduler", claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, itemID := range []string{"7", "8", "9"} {
		if ok, _, err := ledger.Claim(itemID, "curation-run", "backlog-curation", DefaultClaimLease); err != nil || !ok {
			t.Fatalf("seed curation claim %s: ok=%v err=%v", itemID, ok, err)
		}
	}
	if ok, _, err := ledger.Claim("10", "other-run", "implementation", DefaultClaimLease); err != nil || !ok {
		t.Fatalf("seed other run claim: ok=%v err=%v", ok, err)
	}

	t.Chdir(t.TempDir())

	code, stdout, stderr := runArgs(t, "backlog-query", "--release", root)
	if code != 0 {
		t.Fatalf("release: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if stdout != "released 7, 8, 9\n" {
		t.Fatalf("stdout = %q, want every released item", stdout)
	}

	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatalf("reopen claim ledger: %v", err)
	}
	entries := reopened.Snapshot()
	for _, entry := range entries {
		if entry.RunID == "curation-run" {
			t.Fatalf("claim %s leaked for curation-run: %+v", entry.ItemID, entry)
		}
	}
	if len(entries) != 1 || entries[0].ItemID != "10" || entries[0].RunID != "other-run" {
		t.Fatalf("claim ledger = %+v, want only item 10 held by other-run", entries)
	}
	server.mu.Lock()
	for _, itemID := range []int{7, 8, 9} {
		if hasAnyLabel(server.issues[itemID].labels, []string{"goobers:claimed"}) {
			t.Errorf("issue %d labels = %v, want goobers:claimed removed", itemID, server.issues[itemID].labels)
		}
	}
	otherLabels := append([]string(nil), server.issues[10].labels...)
	server.mu.Unlock()
	if !hasAnyLabel(otherLabels, []string{"goobers:claimed"}) {
		t.Fatalf("other run's issue labels = %v, want goobers:claimed preserved", otherLabels)
	}
}

func TestBacklogQueryReleaseRetainsClaimWhenProviderCleanupFails(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_ISSUES_WRITE", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")

	ledgerPath := filepath.Join(root, "scheduler", claimLedgerFileName)
	ledger, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := ledger.Claim("7", "curation-run", "backlog-curation", DefaultClaimLease); err != nil || !ok {
		t.Fatalf("seed curation claim: ok=%v err=%v", ok, err)
	}
	t.Chdir(t.TempDir())

	code, _, stderr := runArgs(t, "backlog-query", "--release", root)
	if code != 1 || !strings.Contains(stderr, "release provider claim marker for 7") {
		t.Fatalf("release: code = %d, stderr = %q, want provider cleanup failure", code, stderr)
	}
	reopened, err := localscheduler.OpenClaimLedger(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	entry, held := reopened.Lookup("7")
	if !held || entry.RunID != "curation-run" {
		t.Fatalf("claim after failed cleanup = %+v, held=%v; want retained for retry", entry, held)
	}
}

// TestBacklogQueryReleaseIsIdempotent is issue #234's crash-resume
// acceptance criterion: releasing a claim the run does not hold (already
// released, or a crash-resume of the release stage itself) is a no-op
// success, not an error — critical since a checkpoint-trust resume of a
// deterministic stage may retry it after its work already durably landed.
func TestBacklogQueryReleaseIsIdempotent(t *testing.T) {
	root := initDemo(t)

	t.Setenv("GOOBERS_RUN_ID", "curation-run")
	t.Setenv("GOOBERS_WORKFLOW", "backlog-curation")
	t.Chdir(t.TempDir())

	// No claim ledger exists at all yet — the run holds nothing.
	code, stdout, stderr := runArgs(t, "backlog-query", "--release", root)
	if code != 0 {
		t.Fatalf("release with nothing held: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "nothing to release") {
		t.Fatalf("stdout = %q, want a clear no-op message", stdout)
	}

	// A second release call (simulating a crash-resume retry) is the same
	// clean no-op, not an error on an already-released claim.
	code, stdout, stderr = runArgs(t, "backlog-query", "--release", root)
	if code != 0 {
		t.Fatalf("second release call: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "nothing to release") {
		t.Fatalf("stdout = %q, want the same no-op message on retry", stdout)
	}
}

// TestBacklogQueryClaimAndReleaseAreMutuallyExclusive proves the CLI-level
// usage guard: --claim and --release together is a usage error, not an
// attempt to do both or a silent pick of one.
func TestBacklogQueryClaimAndReleaseAreMutuallyExclusive(t *testing.T) {
	root := initDemo(t)
	t.Chdir(t.TempDir())

	code, _, _ := runArgs(t, "backlog-query", "--claim", "--release", root)
	if code != 2 {
		t.Fatalf("code = %d, want 2 (usage error)", code)
	}
}

func TestSelectBacklogQueryMode(t *testing.T) {
	tests := []struct {
		name                       string
		readOnly, claim, reconcile bool
		release                    bool
		want                       backlogQueryMode
		ok                         bool
	}{
		{name: "plain", want: backlogQueryModePlain, ok: true},
		{name: "read only", readOnly: true, want: backlogQueryModeReadOnly, ok: true},
		{name: "claim", claim: true, want: backlogQueryModeClaim, ok: true},
		{name: "reconcile", reconcile: true, want: backlogQueryModeReconcile, ok: true},
		{name: "release", release: true, want: backlogQueryModeRelease, ok: true},
		{name: "conflicting modes", claim: true, reconcile: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := selectBacklogQueryMode(test.readOnly, test.claim, test.reconcile, test.release)
			if got != test.want || ok != test.ok {
				t.Fatalf("selectBacklogQueryMode() = (%v, %v), want (%v, %v)", got, ok, test.want, test.ok)
			}
		})
	}
}
