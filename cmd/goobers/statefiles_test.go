package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/stateclient"
)

// These are the direct-file accessors for the scheduler-state keys. Production
// reads and writes them through internal/stateclient (so the same call site
// works on an instance's own volume and through the daemon's state plane), but
// the tests that predate the plane assert against the file itself — which is
// still the store behind both paths, and is exactly what the far-side evidence
// inspects. They live here so the production build carries no path-based
// second way in.

func blockedRecordsPath(l instance.Layout) string {
	return filepath.Join(l.SchedulerDir(), blockedRecordsFileName)
}

func loadBlockedRecords(path string) (map[string]blockedRecord, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]blockedRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	recs := map[string]blockedRecord{}
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return recs, nil
}

func saveBlockedRecords(path string, recs map[string]blockedRecord) error {
	data, err := encodeBlockedRecords(recs)
	if err != nil {
		return err
	}
	// Write-then-rename for the same torn-write reason the claim ledger's own
	// persistence uses: a crash mid-write must never leave a half-written
	// file that fails every subsequent selection tick's parse.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := durability.ReplaceFile(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

func readPostMergeReconcileLedger(path string) (postMergeReconcileLedger, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return emptyPostMergeReconcileLedger(), nil
	}
	if err != nil {
		return emptyPostMergeReconcileLedger(), fmt.Errorf("read post-merge reconcile ledger: %w", err)
	}
	return decodePostMergeReconcileLedger(stateclient.Value{Data: data, ETag: stateclient.ETagFor(data)})
}

func writePostMergeReconcileLedger(path string, ledger postMergeReconcileLedger) error {
	data, err := encodePostMergeReconcileLedger(ledger)
	if err != nil {
		return err
	}
	if err := journal.WriteFileAtomic(path, data, 0o644); err != nil {
		return fmt.Errorf("write post-merge reconcile ledger: %w", err)
	}
	return nil
}
