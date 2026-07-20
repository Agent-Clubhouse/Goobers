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
	"strings"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

const (
	pendingClaimsDir           = "pending-claims"
	claimAdminRequestSuffix    = ".request.json"
	claimAdminResponseSuffix   = ".response.json"
	claimAdminCodeNotFound     = "claim_not_found"
	claimAdminCodeAmbiguous    = "claim_ambiguous"
	claimAdminCodeInvalidScope = "claim_invalid_scope"
	claimAdminOperationList    = "list"
	claimAdminOperationRelease = "release"
)

var claimAdminDelegationTimeout = 30 * time.Second

type claimAdminRequest struct {
	Operation string    `json:"operation"`
	ItemID    string    `json:"itemId,omitempty"`
	Gaggle    string    `json:"gaggle,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type claimAdminResponse struct {
	Entries  []localscheduler.ClaimEntry `json:"entries,omitempty"`
	Released *localscheduler.ClaimEntry  `json:"released,omitempty"`
	Code     string                      `json:"code,omitempty"`
	Error    string                      `json:"error,omitempty"`
}

func runClaims(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) {
		pf(w, "Usage: goobers claims <command> [flags] [path]\n\n"+
			"Inspect and force-release scheduler/claims.json without racing a live\n"+
			"daemon. Operations delegate to `goobers up` when it is running.\n\n"+
			"Commands:\n"+
			"  list       print current claim leases\n"+
			"  release    force-release one item by id\n")
	}
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

func runClaimsList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("claims list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit claim entries as JSON")
	staleOnly := fs.Bool("stale", false, "show only claims whose lease has expired")
	gaggle := fs.String("gaggle", "", "show only claims in this gaggle")
	provider := fs.String("provider", "", "show only claims from this provider")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers claims list [--json] [--stale] [--gaggle=name] [--provider=name] [path]\n\n"+
			"Print item id, gaggle, provider, run id, workflow, claimed-at, and\n"+
			"expires-at for each claim. Filters may be combined. Default path is \".\".\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
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

func runClaimsRelease(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("claims release", flag.ContinueOnError)
	fs.SetOutput(stderr)
	gaggle := fs.String("gaggle", "", "gaggle owning the claim")
	provider := fs.String("provider", "", "provider owning the claim")
	fs.Usage = func() {
		pf(stderr, "Usage: goobers claims release [--gaggle=name --provider=name] <item-id> [path]\n\n"+
			"Force-release an item regardless of the run holding it. The override is\n"+
			"recorded as claim.force_released in the instance journal. Default path\n"+
			"is \".\". Scope flags are required when the item id exists in more than\n"+
			"one namespace. Exit codes: 0 = released, 1 = not found/ambiguous,\n"+
			"2 = usage/IO error.\n")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	if (*gaggle == "") != (*provider == "") {
		pf(stderr, "error: --gaggle and --provider must be supplied together\n")
		return 2
	}
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}

	resp, err := runClaimAdmin(root, claimAdminRequest{
		Operation: claimAdminOperationRelease,
		ItemID:    fs.Arg(0),
		Gaggle:    *gaggle,
		Provider:  *provider,
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
			if err := ledger.ForceReleaseEntry(entry); err != nil {
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
	reqDir := filepath.Join(schedulerDir, pendingClaimsDir)
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		return "", fmt.Errorf("claims delegate: create request dir: %w", err)
	}
	f, err := os.CreateTemp(reqDir, ".pending-*")
	if err != nil {
		return "", fmt.Errorf("claims delegate: create request: %w", err)
	}
	tmpPath := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}

	req.CreatedAt = time.Now().UTC()
	data, err := json.Marshal(req)
	if err != nil {
		cleanup()
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return "", fmt.Errorf("claims delegate: write request: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("claims delegate: close request: %w", err)
	}
	requestID := strings.TrimPrefix(filepath.Base(tmpPath), ".pending-")
	finalPath := filepath.Join(reqDir, requestID+claimAdminRequestSuffix)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("claims delegate: publish request: %w", err)
	}
	return requestID, nil
}

func pollClaimAdminResponse(ctx context.Context, schedulerDir, requestID string, timeout time.Duration) (claimAdminResponse, error) {
	respPath := filepath.Join(schedulerDir, pendingClaimsDir, requestID+claimAdminResponseSuffix)
	deadline := time.Now().Add(timeout)
	for {
		if data, err := os.ReadFile(respPath); err == nil {
			var resp claimAdminResponse
			if err := json.Unmarshal(data, &resp); err == nil {
				_ = os.Remove(respPath)
				return resp, nil
			}
		}
		if time.Now().After(deadline) {
			return claimAdminResponse{}, fmt.Errorf(
				"claims delegate: timed out after %s waiting for the live `goobers up` daemon; "+
					"the operation may have completed, so inspect the claim ledger before retrying (request left at %s)",
				timeout,
				filepath.Join(schedulerDir, pendingClaimsDir, requestID+claimAdminRequestSuffix),
			)
		}
		select {
		case <-ctx.Done():
			return claimAdminResponse{}, ctx.Err()
		case <-time.After(delegationPollInterval):
		}
	}
}

func sweepPendingClaimAdminRequests(schedulerDir string, log *journal.InstanceLog, now func() time.Time) error {
	reqDir := filepath.Join(schedulerDir, pendingClaimsDir)
	entries, err := os.ReadDir(reqDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claims delegate: read pending requests: %w", err)
	}

	var sweepErr error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(reqDir, entry.Name())
		if strings.HasSuffix(entry.Name(), claimAdminResponseSuffix) {
			info, err := entry.Info()
			if err == nil && now().Sub(info.ModTime()) > claimAdminDelegationTimeout {
				_ = os.Remove(path)
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), claimAdminRequestSuffix) {
			continue
		}
		requestID := strings.TrimSuffix(entry.Name(), claimAdminRequestSuffix)
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("claims delegate: read request %s: %w", requestID, err))
			}
			continue
		}
		if err := os.Remove(path); err != nil {
			if !os.IsNotExist(err) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("claims delegate: consume request %s: %w", requestID, err))
			}
			continue
		}

		var req claimAdminRequest
		resp := claimAdminResponse{}
		if err := json.Unmarshal(data, &req); err != nil {
			resp.Error = fmt.Sprintf("claims delegate: malformed request: %v", err)
		} else {
			switch {
			case req.CreatedAt.IsZero():
				resp.Error = "claims delegate: request has no creation time"
			case now().Sub(req.CreatedAt) > claimAdminDelegationTimeout:
				resp.Error = fmt.Sprintf("claims delegate: stale request %s; refusing to execute", requestID)
			default:
				resp, err = executeClaimAdminRequest(schedulerDir, log, req)
				if err != nil {
					resp.Error = err.Error()
				}
			}
		}
		respData, err := json.Marshal(resp)
		if err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("claims delegate: encode response %s: %w", requestID, err))
			continue
		}
		if err := journal.WriteFileAtomic(filepath.Join(reqDir, requestID+claimAdminResponseSuffix), respData, 0o644); err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("claims delegate: write response %s: %w", requestID, err))
		}
	}
	return sweepErr
}
