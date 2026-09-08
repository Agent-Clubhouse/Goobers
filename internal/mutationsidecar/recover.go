package mutationsidecar

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// RecoverBeforeCleanup preserves missing provider receipts in the journal
// selected by the host's ownership marker. Receipt runId is claim ownership,
// not authority to select a destination journal. The sidecar is never deleted
// here; the caller may destroy its worktree only after this returns nil.
func RecoverBeforeCleanup(ctx context.Context, workspace, worktreeID, ownerRunID, journalDir string) (err error) {
	data, err := Read(workspace)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	facts, err := ParseRecoveryFacts(data)
	if err != nil || len(facts) == 0 {
		return err
	}
	if !apiv1.ValidRunID(ownerRunID) || worktreeID == "" {
		return fmt.Errorf("mutation recovery requires durable run and worktree ownership")
	}
	reader, err := journal.OpenReadOnly(journalDir)
	if err != nil {
		return err
	}
	identity, err := reader.Identity()
	if err != nil {
		return err
	}
	if identity.RunID != ownerRunID {
		return fmt.Errorf("mutation recovery journal owner does not match worktree marker")
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	pending, err := missingRecoveryEvents(facts, events, worktreeID)
	if err != nil || len(pending) == 0 {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	writer, report, err := journal.TryRecover(journalDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, writer.Close()) }()
	lockedReader, err := journal.OpenReadOnly(journalDir)
	if err != nil {
		return err
	}
	lockedIdentity, err := lockedReader.Identity()
	if err != nil {
		return err
	}
	if lockedIdentity.RunID != ownerRunID {
		return fmt.Errorf("mutation recovery journal owner changed before writer acquisition")
	}
	// Recheck the authoritative under-lock prefix: another writer may have
	// committed a receipt after the unlocked fast path above.
	pending, err = missingRecoveryEvents(facts, report.Events, worktreeID)
	if err != nil {
		return err
	}
	for _, event := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writer.Append(event); err != nil {
			return err
		}
	}
	return nil
}

func recoveryEvent(fact Fact) journal.Event {
	return journal.WithMutationOutcome(journal.Event{
		Type:        journal.EventRefTouched,
		ExternalRef: &journal.ExternalRef{Provider: fact.Provider, Kind: fact.Kind, ID: fact.ID, URL: fact.URL},
		Runner:      providers.MutationReceiptRunnerFields(fact.ReceiptID, fact.Operation, fact.MergeConfirmation, fact.QueueAdmission, fact.LandingIntent),
	}, fact.RunID, fact.Outcome, fact.ErrorCode, fact.ProviderRunID)
}

func mutationFingerprint(event journal.Event) (string, error) {
	fields := map[string]any{}
	for _, key := range []string{"operation", "mergeConfirmation", "queueAdmission", "landingIntent", "claimRunId", "outcome", "providerRunId"} {
		if value, ok := event.Runner[key]; ok {
			fields[key] = value
		}
	}
	value := struct {
		Type   journal.EventType
		Ref    *journal.ExternalRef
		Error  *journal.ErrorDetail
		Fields map[string]any
	}{event.Type, event.ExternalRef, event.Error, fields}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	// Normalize typed producer structs and decoded journal maps identically.
	var normalized any
	if err := json.Unmarshal(data, &normalized); err != nil {
		return "", err
	}
	data, err = json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func missingRecoveryEvents(facts []Fact, recorded []journal.Event, worktreeID string) ([]journal.Event, error) {
	receipts := map[string]string{}
	for _, event := range recorded {
		if event.ExternalRef == nil || (event.Type != journal.EventRefTouched && event.Type != journal.EventError) {
			continue
		}
		id, _ := event.Runner["mutationReceiptId"].(string)
		if id == "" {
			continue
		}
		fingerprint, _ := event.Runner["mutationRecoveryFingerprint"].(string)
		if fingerprint == "" {
			var err error
			fingerprint, err = mutationFingerprint(event)
			if err != nil {
				return nil, err
			}
		}
		if prior, ok := receipts[id]; ok && prior != fingerprint {
			return nil, fmt.Errorf("conflicting journal mutation receipt identity")
		}
		receipts[id] = fingerprint
	}
	var missing []journal.Event
	for _, fact := range facts {
		if fact.ReceiptID == "" {
			return nil, fmt.Errorf("legacy mutation receipt has no durable identity; preserving worktree")
		}
		event := recoveryEvent(fact)
		fingerprint, err := mutationFingerprint(event)
		if err != nil {
			return nil, err
		}
		if prior, ok := receipts[fact.ReceiptID]; ok {
			if prior != fingerprint {
				return nil, fmt.Errorf("conflicting mutation receipt identity; preserving worktree")
			}
			continue
		}
		receipts[fact.ReceiptID] = fingerprint
		event.Runner["mutationRecoveryFingerprint"] = fingerprint
		event.Runner["recoveredFromWorktree"] = worktreeID
		missing = append(missing, event)
	}
	return missing, nil
}
