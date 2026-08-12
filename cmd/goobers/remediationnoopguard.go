package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

const (
	remediationNoopStateFile = "pr-remediation-noop.json"
	remediationNoopLimit     = 2
)

type remediationNoopSignature struct {
	HeadSHA    string `json:"headSha"`
	Causes     string `json:"causes,omitempty"`
	DiffDigest string `json:"diffDigest,omitempty"`
}

type remediationNoopRecord struct {
	remediationNoopSignature
	Attempts  int    `json:"attempts"`
	LastRunID string `json:"lastRunId"`
	Parked    bool   `json:"parked,omitempty"`
}

type remediationNoopState struct {
	Records map[string]remediationNoopRecord `json:"records"`
}

type remediationNoopUpdate struct {
	remediationNoopSignature
	implementSucceeded bool
}

func remediationNoopKey(gaggle string, number int) string {
	return gaggle + "/" + pullRequestClaimKey(number)
}

func normalizeRemediationCauses(raw string) string {
	parts := splitLabelList(raw)
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func recordPRRemediationNoop(l instance.Layout, runID string) error {
	update, err := preparePRRemediationNoopUpdate(l, runID)
	if err != nil || update == nil {
		return err
	}
	return withClaimLockForRun(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationPRRelease, l.Gaggle(), runID, func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		return recordPRRemediationNoopLocked(l, ledger, runID, *update)
	})
}

func preparePRRemediationNoopUpdate(l instance.Layout, runID string) (*remediationNoopUpdate, error) {
	runDir, err := instance.NewLayout(l.Root).FindRunDir(runID)
	if err != nil {
		return nil, fmt.Errorf("find terminal run journal: %w", err)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		return nil, err
	}
	if _, err := reader.Identity(); err != nil {
		return nil, err
	}
	events, err := reader.Events()
	if err != nil {
		return nil, err
	}

	var update remediationNoopUpdate
	implementNoWork := false
	for _, event := range events {
		if event.Type != journal.EventStageFinished {
			continue
		}
		switch event.Stage {
		case "rebase-pr":
			update.HeadSHA = scalarString(event.Outputs["attemptedHeadSha"])
			update.Causes = normalizeRemediationCauses(scalarString(event.Outputs["remediationCauses"]))
		case "implement":
			switch event.Status {
			case string(apiv1.ResultSuccess):
				update.implementSucceeded = true
			case string(apiv1.ResultNoWork):
				implementNoWork = true
			}
		}
	}
	if !update.implementSucceeded && !implementNoWork {
		return nil, nil
	}
	if !update.implementSucceeded && (update.HeadSHA == "" || update.Causes == "") {
		return nil, nil
	}
	return &update, nil
}

func recordPRRemediationNoopLocked(l instance.Layout, ledger *localscheduler.ClaimLedger, runID string, update remediationNoopUpdate) error {
	for _, entry := range ledger.ForRunAll(runID) {
		if !strings.HasPrefix(entry.ItemID, pullRequestClaimPrefix) {
			continue
		}
		number, err := strconv.Atoi(strings.TrimPrefix(entry.ItemID, pullRequestClaimPrefix))
		if err != nil {
			return fmt.Errorf("parse PR claim %q: %w", entry.ItemID, err)
		}
		key := remediationNoopKey(entry.Gaggle, number)
		if update.implementSucceeded {
			return clearRemediationNoopState(l.SchedulerDir(), key)
		}
		return updateRemediationNoopState(l.SchedulerDir(), key, update.remediationNoopSignature, runID)
	}
	return nil
}

func scalarString(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return ""
	}
}

func updateRemediationNoopState(schedulerDir, key string, signature remediationNoopSignature, runID string) error {
	state, err := readRemediationNoopState(schedulerDir)
	if err != nil {
		return err
	}
	record := state.Records[key]
	if record.remediationNoopSignature != signature {
		record = remediationNoopRecord{remediationNoopSignature: signature}
	}
	if record.LastRunID == runID {
		return nil
	}
	record.Attempts++
	record.LastRunID = runID
	state.Records[key] = record
	return writeRemediationNoopState(schedulerDir, state)
}

func recordGatherPRContextDigestNoop(l instance.Layout, number int, signature remediationNoopSignature, runID string, escalatedLabelPresent bool) (remediationNoopRecord, bool, error) {
	if runID == "" {
		return remediationNoopRecord{}, false, fmt.Errorf("GOOBERS_RUN_ID is required to record an unchanged remediation digest")
	}
	var recorded remediationNoopRecord
	var operatorReset bool
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationPRLookup, func() error {
		state, err := readRemediationNoopState(l.SchedulerDir())
		if err != nil {
			return err
		}
		gaggle := l.Gaggle()
		if gaggle == "" {
			gaggle = providerGaggle()
		}
		key := remediationNoopKey(gaggle, number)
		record := state.Records[key]
		if record.remediationNoopSignature == signature && record.Parked && !escalatedLabelPresent {
			delete(state.Records, key)
			operatorReset = true
			return writeRemediationNoopState(l.SchedulerDir(), state)
		}
		if record.remediationNoopSignature != signature {
			record = remediationNoopRecord{remediationNoopSignature: signature}
		}
		if record.LastRunID != runID {
			record.Attempts++
			record.LastRunID = runID
			state.Records[key] = record
			if err := writeRemediationNoopState(l.SchedulerDir(), state); err != nil {
				return err
			}
		}
		recorded = record
		return nil
	})
	return recorded, operatorReset, err
}

func clearRemediationNoopState(schedulerDir, key string) error {
	state, err := readRemediationNoopState(schedulerDir)
	if err != nil {
		return err
	}
	if _, ok := state.Records[key]; !ok {
		return nil
	}
	delete(state.Records, key)
	return writeRemediationNoopState(schedulerDir, state)
}

func remediationNoopRecordForSignature(l instance.Layout, number int, signature remediationNoopSignature) (remediationNoopRecord, error) {
	var matched remediationNoopRecord
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationPRLookup, func() error {
		state, err := readRemediationNoopState(l.SchedulerDir())
		if err != nil {
			return err
		}
		gaggle := l.Gaggle()
		if gaggle == "" {
			gaggle = providerGaggle()
		}
		key := remediationNoopKey(gaggle, number)
		record, ok := state.Records[key]
		if !ok {
			return nil
		}
		if record.remediationNoopSignature != signature {
			delete(state.Records, key)
			return writeRemediationNoopState(l.SchedulerDir(), state)
		}
		matched = record
		return nil
	})
	return matched, err
}

func markRemediationNoopParked(l instance.Layout, key string) error {
	return withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationPRLookup, func() error {
		state, err := readRemediationNoopState(l.SchedulerDir())
		if err != nil {
			return err
		}
		record, ok := state.Records[key]
		if !ok || record.Parked {
			return nil
		}
		record.Parked = true
		state.Records[key] = record
		return writeRemediationNoopState(l.SchedulerDir(), state)
	})
}

func clearRemediationNoopRecord(l instance.Layout, key string) error {
	return withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationPRLookup, func() error {
		return clearRemediationNoopState(l.SchedulerDir(), key)
	})
}

func readRemediationNoopState(schedulerDir string) (remediationNoopState, error) {
	path := filepath.Join(schedulerDir, remediationNoopStateFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return remediationNoopState{Records: make(map[string]remediationNoopRecord)}, nil
	}
	if err != nil {
		return remediationNoopState{}, fmt.Errorf("read remediation no-op state: %w", err)
	}
	var state remediationNoopState
	if err := json.Unmarshal(data, &state); err != nil {
		return remediationNoopState{}, fmt.Errorf("decode remediation no-op state: %w", err)
	}
	if state.Records == nil {
		state.Records = make(map[string]remediationNoopRecord)
	}
	return state, nil
}

func writeRemediationNoopState(schedulerDir string, state remediationNoopState) error {
	if err := os.MkdirAll(schedulerDir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := journal.WriteFileAtomic(filepath.Join(schedulerDir, remediationNoopStateFile), data, 0o644); err != nil {
		return fmt.Errorf("write remediation no-op state: %w", err)
	}
	return nil
}
