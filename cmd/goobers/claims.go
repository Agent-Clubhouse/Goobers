package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
)

const (
	pendingClaimsDir           = "pending-claims"
	claimAdminRequestSuffix    = ".request.json"
	claimAdminResponseSuffix   = ".response.json"
	claimAdminCodeNotFound     = "claim_not_found"
	claimAdminCodeAmbiguous    = "claim_ambiguous"
	claimAdminCodeInvalidScope = "claim_invalid_scope"
	claimAdminCodeLiveHolder   = "claim_live_holder"
	claimAdminCodeChanged      = "claim_changed"
	claimAdminOperationList    = "list"
	claimAdminOperationRelease = "release"
	// claimAdminOperationRecover is the whole-instance stale-claim SWEEP,
	// delegated rather than executed (Goobers#4029). Unlike list and release
	// it never runs in the requesting process: the sweep's inputs — the
	// active-intervention set and the restart-time recovery gate — are
	// in-memory daemon state, so a request for it is only ever answered by
	// the daemon's own recoverExpiredClaims closure in
	// sweepPendingClaimAdminRequests, never by executeClaimAdminRequest.
	claimAdminOperationRecover = "recover"
	claimAdminActorCLI         = "cli"
)

var claimAdminDelegationTimeout = 30 * time.Second

func claimAdminDelegateFileProtocol() delegateFileProtocol {
	return delegateFileProtocol{
		pendingDir:                  pendingClaimsDir,
		requestSuffix:               claimAdminRequestSuffix,
		responseSuffix:              claimAdminResponseSuffix,
		errorPrefix:                 "claims delegate",
		staleAfter:                  claimAdminDelegationTimeout,
		distinguishNonDirectoryPath: true,
	}
}

type claimAdminRequest struct {
	Operation         string    `json:"operation"`
	ItemID            string    `json:"itemId,omitempty"`
	Gaggle            string    `json:"gaggle,omitempty"`
	Provider          string    `json:"provider,omitempty"`
	ExpectedRunID     string    `json:"expectedRunId,omitempty"`
	ExpectedClaimedAt time.Time `json:"expectedClaimedAt,omitempty"`
	Force             bool      `json:"force,omitempty"`
	Actor             string    `json:"actor,omitempty"`
	CreatedAt         time.Time `json:"createdAt"`
}

type claimAdminResponse struct {
	Entries  []localscheduler.ClaimEntry `json:"entries,omitempty"`
	Released *localscheduler.ClaimEntry  `json:"released,omitempty"`
	// Recovered carries what a delegated claimAdminOperationRecover sweep
	// released. Separate from Entries (a list result) and Released (one
	// force-released lease) so a reader never has to know which operation
	// produced the response to read it correctly.
	Recovered []localscheduler.ClaimEntry `json:"recovered,omitempty"`
	Code      string                      `json:"code,omitempty"`
	Error     string                      `json:"error,omitempty"`
}

const claimsHelp = "Usage: goobers claims <command> [flags] [path]\n\n" +
	"Inspect and force-release scheduler/claims.json without racing a live\n" +
	"daemon. Operations delegate to `goobers up` when it is running.\n\n" +
	"Commands:\n" +
	"  list       print current claim leases\n" +
	"  active     print what is actively claimed now\n" +
	"  release    force-release one item by id\n"

func runClaims(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) { pf(w, "%s", claimsHelp) }
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		usage(stdout)
		return 0
	}
	if len(args) > 0 {
		pf(stderr, "error: unknown claims command %q\n", args[0])
	}
	usage(stderr)
	return 2
}

const claimsListHelp = "Usage: goobers claims list [--json] [--stale] [--gaggle=name] [--provider=name] [path]\n\n" +
	"Print item id, gaggle, provider, run id, workflow, claimed-at, and\n" +
	"expires-at for each claim. Filters may be combined. Default path is \".\".\n"

func runClaimsList(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("claims list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit claim entries as JSON")
	staleOnly := fs.Bool("stale", false, "show only claims whose lease has expired")
	gaggle := fs.String("gaggle", "", "show only claims in this gaggle")
	provider := fs.String("provider", "", "show only claims from this provider")
	fs.Usage = helpUsage(stderr, "claims list")
	root, ok := parseOptionalRoot(fs, args)
	if !ok {
		return 2
	}

	resp, err := runClaimAdmin(root, claimAdminRequest{
		Operation: claimAdminOperationList,
		Gaggle:    *gaggle,
		Provider:  *provider,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if resp.Error != "" {
		pf(stderr, "error: %s\n", resp.Error)
		return 2
	}
	entries := resp.Entries
	if entries == nil {
		entries = []localscheduler.ClaimEntry{}
	}
	if *staleOnly {
		now := time.Now()
		filtered := entries[:0]
		for _, entry := range entries {
			if !entry.ExpiresAt.After(now) {
				filtered = append(filtered, entry)
			}
		}
		entries = filtered
	}

	if *jsonOutput {
		for i := range entries {
			entries[i].Verification = entries[i].Verification.Report()
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(entries); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		return 0
	}
	if len(entries) == 0 {
		if *staleOnly {
			pln(stdout, "no stale claims")
		} else {
			pln(stdout, "no claims")
		}
		return 0
	}
	pln(stdout, "ITEM ID\tGAGGLE\tPROVIDER\tRUN ID\tWORKFLOW\tCLAIMED AT\tEXPIRES AT")
	for _, entry := range entries {
		pf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			entry.ItemID,
			claimScopeValue(entry.Gaggle),
			claimScopeValue(entry.Provider),
			entry.RunID,
			entry.Workflow,
			entry.ClaimedAt.UTC().Format(time.RFC3339),
			entry.ExpiresAt.UTC().Format(time.RFC3339),
		)
	}
	return 0
}

const claimsActiveHelp = "Usage: goobers claims active [--json] [--gaggle=name] [--provider=name] [path]\n\n" +
	"Print what this instance has actively claimed now: item, workflow, run,\n" +
	"holder, and age, oldest first. Expired, released, and revoked leases are\n" +
	"omitted (`goobers claims list --stale` shows expired ones). Holder is the owning instance\n" +
	"for a shared-visibility claim, otherwise \"local\". The daemon API serves\n" +
	"the same view at GET /api/v1/claims/active. Default path is \".\".\n"

func runClaimsActive(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("claims active", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit active claims as JSON")
	gaggle := fs.String("gaggle", "", "show only claims in this gaggle")
	provider := fs.String("provider", "", "show only claims from this provider")
	fs.Usage = helpUsage(stderr, "claims active")
	root, ok := parseOptionalRoot(fs, args)
	if !ok {
		return 2
	}

	resp, err := runClaimAdmin(root, claimAdminRequest{
		Operation: claimAdminOperationList,
		Gaggle:    *gaggle,
		Provider:  *provider,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if resp.Error != "" {
		pf(stderr, "error: %s\n", resp.Error)
		return 2
	}
	now := time.Now().UTC()
	view := readservice.ActiveClaimList{ObservedAt: now, Claims: readservice.ActiveClaimsFromEntries(resp.Entries, now)}

	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(view); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		return 0
	}
	if len(view.Claims) == 0 {
		pln(stdout, "no active claims")
		return 0
	}
	pln(stdout, "ITEM ID\tGAGGLE\tPROVIDER\tWORKFLOW\tRUN ID\tHOLDER\tAGE")
	for _, claim := range view.Claims {
		pf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			claim.ItemID,
			claimScopeValue(claim.Gaggle),
			claimScopeValue(claim.Provider),
			claim.Workflow,
			claim.RunID,
			claim.Holder,
			(time.Duration(claim.AgeSeconds) * time.Second).String(),
		)
	}
	return 0
}

const claimsReleaseHelp = "Usage: goobers claims release [--force] [--gaggle=name --provider=name] <item-id> [path]\n\n" +
	"Print the claim holder, age, and expiry, then force-release the item.\n" +
	"--force is required while the holding run is non-terminal. The override\n" +
	"is recorded as claim.force_released in the instance journal. Default\n" +
	"path is \".\". Scope flags are required when the item id exists in more\n" +
	"than one namespace. Exit codes: 0 = released, 1 = refused/not found/\n" +
	"ambiguous, 2 = usage/IO error.\n"

func runClaimsRelease(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("claims release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gaggle := fs.String("gaggle", "", "gaggle owning the claim")
	provider := fs.String("provider", "", "provider owning the claim")
	force := fs.Bool("force", false, "release a claim held by a non-terminal run")
	fs.Usage = helpUsage(stderr, "claims release")
	itemID, root, ok := parseRequiredArgOptionalRoot(fs, args)
	if !ok {
		return 2
	}
	if (*gaggle == "") != (*provider == "") {
		pf(stderr, "error: --gaggle and --provider must be supplied together\n")
		return 2
	}

	if err := prepareManualRoot(instance.NewLayout(root), stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	previewResp, err := runClaimAdmin(root, claimAdminRequest{
		Operation: claimAdminOperationList,
		Gaggle:    *gaggle,
		Provider:  *provider,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if previewResp.Error != "" {
		pf(stderr, "error: %s\n", previewResp.Error)
		return 2
	}
	entry, code, message := selectClaimForRelease(previewResp.Entries, claimAdminRequest{
		ItemID:   itemID,
		Gaggle:   *gaggle,
		Provider: *provider,
	})
	if code != "" {
		pf(stderr, "error: %s\n", message)
		return 1
	}
	printClaimReleasePreview(stdout, entry, time.Now())

	terminal, err := claimHolderTerminal(root, entry)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if !terminal && !*force {
		pf(stderr, "error: claim is held by non-terminal run %s; rerun with --force to release it\n", entry.RunID)
		return 1
	}

	// Tier 1 local filesystem access is the authorization boundary. Route this
	// mutation through the tier-2 access-control seam when #172/#469 lands.
	stopTelemetry := startCommandJournalTelemetry(instance.NewLayout(root), stderr)
	defer stopTelemetry()
	resp, err := runClaimAdmin(root, claimAdminRequest{
		Operation:         claimAdminOperationRelease,
		ItemID:            itemID,
		Gaggle:            *gaggle,
		Provider:          *provider,
		ExpectedRunID:     entry.RunID,
		ExpectedClaimedAt: entry.ClaimedAt,
		Force:             *force,
		Actor:             claimAdminActorCLI,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if resp.Code != "" {
		pf(stderr, "error: %s\n", resp.Error)
		return 1
	}
	if resp.Error != "" {
		pf(stderr, "error: %s\n", resp.Error)
		return 2
	}
	if resp.Released == nil {
		pf(stderr, "error: daemon returned no released claim\n")
		return 2
	}
	pf(stdout, "released claim %s (was held by run %s, workflow %s)\n",
		resp.Released.ItemID, resp.Released.RunID, resp.Released.Workflow)
	return 0
}

func printClaimReleasePreview(w io.Writer, entry localscheduler.ClaimEntry, now time.Time) {
	age := now.Sub(entry.ClaimedAt)
	if age < 0 {
		age = 0
	}
	pf(w, "claim %s (gaggle %s, provider %s): holder run %s, workflow %s, age %s, expires %s\n",
		entry.ItemID,
		claimScopeValue(entry.Gaggle),
		claimScopeValue(entry.Provider),
		entry.RunID,
		entry.Workflow,
		age.Round(time.Second),
		entry.ExpiresAt.UTC().Format(time.RFC3339),
	)
}

func claimHolderTerminal(root string, entry localscheduler.ClaimEntry) (bool, error) {
	runID := entry.RunID
	if owner, ok := parseBacklogReconcileRunID(runID); ok {
		// A backlog-reconcile claim's holder is the reconcile invocation
		// itself (backlogreconcile.go's synthesized "<owner-run>/
		// backlog-reconcile/<pid>/<seq>" RunID), not a run FindRunDir can
		// look up — it is terminal iff the run that spawned it is.
		runID = owner
	} else if strings.Contains(runID, "/") {
		// Some other slash-containing (and therefore, per FindRunDir,
		// structurally invalid) run id — including a reconcile-shaped id
		// whose pid/sequence suffix failed to parse. Hold conservatively
		// rather than surface FindRunDir's rejection as an inspection error
		// on every recovery sweep for as long as the entry lives (bounded
		// by its own TTL either way).
		return false, nil
	}
	runDir, err := instance.NewLayout(root).FindRunDir(runID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect holding run %s: %w", entry.RunID, err)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		return false, fmt.Errorf("inspect holding run %s: %w", entry.RunID, err)
	}
	phase, err := reader.Phase()
	if err != nil {
		return false, fmt.Errorf("read holding run %s journal: %w", entry.RunID, err)
	}
	return phase != journal.PhaseRunning, nil
}

func claimScopeValue(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func runClaimAdmin(root string, req claimAdminRequest) (claimAdminResponse, error) {
	l := instance.NewLayout(root)
	if _, err := os.Stat(l.ConfigFile()); err != nil {
		return claimAdminResponse{}, fmt.Errorf("%s not found (not an instance root -- run `goobers init` first)", l.ConfigFile())
	}
	if err := os.MkdirAll(l.SchedulerDir(), 0o755); err != nil {
		return claimAdminResponse{}, err
	}

	running, _, err := inspectDaemonLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		return claimAdminResponse{}, err
	}
	if !running {
		if req.Operation == claimAdminOperationList {
			return executeClaimAdminRequest(l.SchedulerDir(), nil, req)
		}
		log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
		if err != nil {
			return claimAdminResponse{}, err
		}
		resp, executeErr := executeClaimAdminRequest(l.SchedulerDir(), log, req)
		closeErr := log.Close()
		return resp, errors.Join(executeErr, closeErr)
	}
	requestID, err := writeClaimAdminRequest(l.SchedulerDir(), req)
	if err != nil {
		return claimAdminResponse{}, err
	}
	return pollClaimAdminResponse(context.Background(), l.SchedulerDir(), requestID, claimAdminDelegationTimeout)
}

func executeClaimAdminRequest(schedulerDir string, log *journal.InstanceLog, req claimAdminRequest) (claimAdminResponse, error) {
	var resp claimAdminResponse
	operation := claimLockOperationAdminList
	if req.Operation == claimAdminOperationRelease {
		operation = claimLockOperationAdminRelease
	}
	err := withClaimLock(filepath.Join(schedulerDir, claimLockFileName), operation, func() error {
		opts := []localscheduler.LedgerOption{}
		if log != nil {
			opts = append(opts, localscheduler.WithInstanceLog(log))
		}
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName), opts...)
		if err != nil {
			return err
		}
		switch req.Operation {
		case claimAdminOperationList:
			resp.Entries = filterClaimEntries(ledger.Snapshot(), req)
		case claimAdminOperationRelease:
			entry, code, message := selectClaimForRelease(ledger.Snapshot(), req)
			if code != "" {
				resp.Code = code
				resp.Error = message
				return nil
			}
			if req.ExpectedRunID != "" && entry.RunID != req.ExpectedRunID {
				resp.Code = claimAdminCodeChanged
				resp.Error = fmt.Sprintf("claim holder changed from run %s to run %s; inspect and retry", req.ExpectedRunID, entry.RunID)
				return nil
			}
			if !req.ExpectedClaimedAt.IsZero() && !entry.ClaimedAt.Equal(req.ExpectedClaimedAt) {
				resp.Code = claimAdminCodeChanged
				resp.Error = "claim lease changed after inspection; inspect and retry"
				return nil
			}
			if req.Actor != claimAdminActorCLI {
				return fmt.Errorf("claims: release actor must be %q", claimAdminActorCLI)
			}
			terminal, err := claimHolderTerminal(filepath.Dir(schedulerDir), entry)
			if err != nil {
				return err
			}
			if !terminal && !req.Force {
				resp.Code = claimAdminCodeLiveHolder
				resp.Error = fmt.Sprintf("claim is held by non-terminal run %s; --force is required", entry.RunID)
				return nil
			}
			layout := instance.NewLayout(filepath.Dir(schedulerDir))
			if err := forceReleaseClaim(context.Background(), ledger, localLifecycleSharedClaimResolver(layout), entry, req.Actor); err != nil {
				return err
			}
			resp.Released = &entry
		default:
			return fmt.Errorf("claims: unknown admin operation %q", req.Operation)
		}
		return nil
	})
	return resp, err
}

// daemonStaleClaimSweep is the daemon's OWN stale-claim sweep — up.go's
// recoverExpiredClaims, which closes over the active-intervention predicate
// and the restart-time recovery gate. It is injected into the delegation
// sweeper for the same reason daemonClaimService takes it (writeplanes.go):
// those two inputs are in-memory daemon state that no other assembly can
// reconstruct, so a server without the closure answers "unavailable" instead
// of quietly running a weaker sweep.
type daemonStaleClaimSweep func(now time.Time) ([]localscheduler.ClaimEntry, error)

// executeDelegatedStaleClaimSweep answers one claimAdminOperationRecover
// request. It is deliberately NOT part of executeClaimAdminRequest: that
// function's whole body runs inside withClaimLock, and the sweep takes the
// claims lock itself — routing recovery through it would deadlock the daemon
// against its own flock.
//
// A claims-lock timeout is swallowed exactly as the daemon's own startup and
// ticker call sites swallow it, and as daemonClaimService.Recover does on the
// plane: a sweep that could not get the lock this pass is deferred work, not
// a failure to report to the delegating stage.
func executeDelegatedStaleClaimSweep(sweep daemonStaleClaimSweep, now time.Time) (claimAdminResponse, error) {
	if sweep == nil {
		return claimAdminResponse{}, errors.New("claims delegate: this daemon does not run claim recovery")
	}
	released, err := sweep(now)
	if err != nil && !isJournaledClaimsLockTimeout(err) {
		return claimAdminResponse{}, err
	}
	return claimAdminResponse{Recovered: released}, nil
}

func filterClaimEntries(entries []localscheduler.ClaimEntry, req claimAdminRequest) []localscheduler.ClaimEntry {
	filtered := make([]localscheduler.ClaimEntry, 0, len(entries))
	for _, entry := range entries {
		if req.Gaggle != "" && entry.Gaggle != req.Gaggle {
			continue
		}
		if req.Provider != "" && entry.Provider != req.Provider {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func selectClaimForRelease(entries []localscheduler.ClaimEntry, req claimAdminRequest) (localscheduler.ClaimEntry, string, string) {
	if (req.Gaggle == "") != (req.Provider == "") {
		return localscheduler.ClaimEntry{}, claimAdminCodeInvalidScope, "--gaggle and --provider must be supplied together"
	}
	matches := make([]localscheduler.ClaimEntry, 0, 1)
	for _, entry := range entries {
		externalID := entry.ExternalID
		if externalID == "" {
			externalID = entry.ItemID
		}
		if externalID != req.ItemID {
			continue
		}
		if req.Gaggle != "" && (entry.Gaggle != req.Gaggle || entry.Provider != req.Provider) {
			continue
		}
		matches = append(matches, entry)
	}
	switch len(matches) {
	case 0:
		return localscheduler.ClaimEntry{}, claimAdminCodeNotFound, fmt.Sprintf("no claim for item %q", req.ItemID)
	case 1:
		return matches[0], "", ""
	default:
		return localscheduler.ClaimEntry{}, claimAdminCodeAmbiguous,
			fmt.Sprintf("item %q is claimed in multiple namespaces; specify --gaggle and --provider", req.ItemID)
	}
}

func writeClaimAdminRequest(schedulerDir string, req claimAdminRequest) (string, error) {
	return writeDelegateRequest(schedulerDir, claimAdminDelegateFileProtocol(), req, func(req *claimAdminRequest) {
		req.CreatedAt = time.Now().UTC()
	})
}

func pollClaimAdminResponse(ctx context.Context, schedulerDir, requestID string, timeout time.Duration) (claimAdminResponse, error) {
	return pollDelegateResponse[claimAdminResponse](
		ctx, schedulerDir, requestID, claimAdminDelegateFileProtocol(), timeout,
		func(requestPath string) string {
			return fmt.Sprintf(
				"claims delegate: timed out after %s waiting for the live `goobers up` daemon; "+
					"the operation may have completed, so inspect the claim ledger before retrying (request left at %s)",
				timeout,
				requestPath,
			)
		},
	)
}

// startClaimAdminSweep keeps delegated claim operations available through the
// daemon's run-drain phase, after its admission lifecycle has stopped.
func startClaimAdminSweep(
	l instance.Layout,
	log *journal.InstanceLog,
	recover daemonStaleClaimSweep,
	reporter *sweepErrorReporter,
) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := startPeriodicSweep(ctx, delegationSweepInterval, func() {
		reporter.report(sweepPendingClaimAdminRequests(l.SchedulerDir(), log, time.Now, recover))
	})
	return func() {
		cancel()
		<-done
	}
}

func sweepPendingClaimAdminRequests(
	schedulerDir string,
	log *journal.InstanceLog,
	now func() time.Time,
	recover daemonStaleClaimSweep,
) error {
	return sweepDelegateRequests(
		schedulerDir,
		claimAdminDelegateFileProtocol(),
		now,
		func(requestID string, req claimAdminRequest, decodeErr error) (claimAdminResponse, bool) {
			switch {
			case decodeErr != nil:
				return claimAdminResponse{Error: fmt.Sprintf("claims delegate: malformed request: %v", decodeErr)}, false
			case req.CreatedAt.IsZero():
				return claimAdminResponse{Error: "claims delegate: request has no creation time"}, false
			case now().Sub(req.CreatedAt) > claimAdminDelegationTimeout:
				return claimAdminResponse{Error: fmt.Sprintf("claims delegate: stale request %s; refusing to execute", requestID)}, false
			default:
				return claimAdminResponse{}, true
			}
		},
		func(req claimAdminRequest) claimAdminResponse {
			var (
				resp claimAdminResponse
				err  error
			)
			if req.Operation == claimAdminOperationRecover {
				resp, err = executeDelegatedStaleClaimSweep(recover, now())
			} else {
				resp, err = executeClaimAdminRequest(schedulerDir, log, req)
			}
			if err != nil {
				resp.Error = err.Error()
			}
			return resp
		},
	)
}
