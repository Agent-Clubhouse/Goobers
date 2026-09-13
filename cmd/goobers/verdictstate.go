package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// verdictstate.go is the verdict-json half of Goobers#3025/#5030: the
// merge-review verdict that drives pr-remediation's routing/budgets, moved
// off the `<!-- verdict-json: ... -->` PR-comment payload onto the same
// closed scheduler-state KV plane failurestreakstate.go uses for the
// failure-streak half.
//
// The PR comment stays (renderVerdictComment): it is still the only
// cross-repository-provider-visible record a human or an Azure DevOps PR
// thread reader sees, and Gitea/GitHub tooling that renders it. It is written
// AFTER the authoritative key update succeeds, so it is a projection —
// editing or deleting it can no longer change what gatherPRVerdict returns
// once the key exists.
//
// gatherPRVerdict, this file's reader, is unchanged in its comment-parsing
// fallback: an absent key parses the legacy `<!-- verdict-json: ... -->`
// marker exactly as before (parseVerdictComment) and opportunistically
// migrates the result into the key, so every existing comment-driven test
// fixture continues to exercise the same code path unless it sets up a key.
const (
	verdictStateSchema        = "goobers.dev/remediation-verdict/v1"
	verdictStateLockOperation = "remediation-verdict.update"
)

// verdictDocument is one PR's verdict as it lives at its own scheduler-state
// key, carrying the record key it was written for so a mis-keyed document is
// caught rather than acted on (the same integrity posture
// remediationNoopDocument and failureStreakDocument take).
type verdictDocument struct {
	Schema  string        `json:"schema"`
	Key     string        `json:"key"`
	Verdict apiv1.Verdict `json:"verdict"`
}

// verdictRecordKey is the record's logical identity: the repository's
// provider-complete canonical identity plus the PR number, so identically
// numbered PRs in different repositories or providers never collide.
func verdictRecordKey(repo providers.RepositoryRef, prNumber int) string {
	return repo.CanonicalKey() + "#" + strconv.Itoa(prNumber)
}

// verdictStateKey is the scheduler-state key holding one PR's verdict
// record.
func verdictStateKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return stateclient.RemediationVerdictKey(fmt.Sprintf("%x", sum))
}

func decodeVerdictRecord(value stateclient.Value, key string) (apiv1.Verdict, bool, error) {
	if !value.Exists() {
		return apiv1.Verdict{}, false, nil
	}
	var doc verdictDocument
	if err := json.Unmarshal(value.Data, &doc); err != nil {
		return apiv1.Verdict{}, false, fmt.Errorf("decode remediation-verdict state: %w", err)
	}
	if doc.Schema != verdictStateSchema {
		return apiv1.Verdict{}, false, fmt.Errorf(
			"decode remediation-verdict state: unsupported schema %q, want %q", doc.Schema, verdictStateSchema)
	}
	if doc.Key != key {
		return apiv1.Verdict{}, false, fmt.Errorf(
			"decode remediation-verdict state: record is keyed to %q, not %q", doc.Key, key)
	}
	return doc.Verdict, true, nil
}

func encodeVerdictRecord(key string, verdict apiv1.Verdict) ([]byte, error) {
	return json.Marshal(verdictDocument{Schema: verdictStateSchema, Key: key, Verdict: verdict})
}

// updateVerdictRecord is the record's read-modify-write: one lock acquisition
// on the file backend, one compare-and-swap on the plane. fn returns
// write=false to leave the key untouched, and MUST be safe to run more than
// once — the plane re-runs it against the new value when a CAS loses.
func updateVerdictRecord(
	ctx context.Context,
	store stateclient.Store,
	key string,
	fn func(apiv1.Verdict, bool) (apiv1.Verdict, bool, error),
) error {
	return store.Update(ctx, verdictStateKey(key), verdictStateLockOperation,
		func(value stateclient.Value) ([]byte, bool, error) {
			current, exists, err := decodeVerdictRecord(value, key)
			if err != nil {
				return nil, false, err
			}
			next, write, err := fn(current, exists)
			if err != nil || !write {
				return nil, false, err
			}
			data, err := encodeVerdictRecord(key, next)
			if err != nil {
				return nil, false, err
			}
			return data, true, nil
		})
}

// loadVerdictState reads a PR's authoritative verdict record. It answers
// ok=false when the key is absent — that is the caller's cue to fall back to
// parsing the legacy comment marker and migrate on read, exactly as
// loadFailureStreakState does for the failure-streak half.
func loadVerdictState(root string, repo providers.RepositoryRef, prNumber int) (apiv1.Verdict, bool, error) {
	store, err := openStageStateStore(instance.NewLayout(root))
	if err != nil {
		return apiv1.Verdict{}, false, fmt.Errorf("open remediation-verdict state: %w", err)
	}
	key := verdictRecordKey(repo, prNumber)
	value, err := store.Get(stateContext(), verdictStateKey(key))
	if err != nil {
		return apiv1.Verdict{}, false, fmt.Errorf("read remediation-verdict state for PR #%d: %w", prNumber, err)
	}
	return decodeVerdictRecord(value, key)
}

// migrateVerdictState writes verdict into the key only if it is still
// absent — a concurrent writer's value wins — used by gatherPRVerdict's
// migration-on-read path. Best-effort: a failure to migrate is not fatal to
// the caller, which already has the legacy-parsed verdict to fall back on.
func migrateVerdictState(root string, repo providers.RepositoryRef, prNumber int, verdict apiv1.Verdict) {
	store, err := openStageStateStore(instance.NewLayout(root))
	if err != nil {
		return
	}
	key := verdictRecordKey(repo, prNumber)
	_ = updateVerdictRecord(stateContext(), store, key, func(current apiv1.Verdict, exists bool) (apiv1.Verdict, bool, error) {
		if exists {
			return current, false, nil
		}
		return verdict, true, nil
	})
}

// prepareVerdictComment validates verdict, writes it to the authoritative KV
// key, and renders the comment projection — in that order, so the sticky
// status comment is never posted for a verdict that failed validation or
// could not be persisted. Consolidated into one call (rather than three
// separate branches at the call site) to keep runApplyVerdict's own
// complexity flat: the three checks collapse into the single error this
// returns.
func prepareVerdictComment(root string, repo providers.RepositoryRef, selectedNumber int, verdict apiv1.Verdict, scopeGateParked bool) (string, error) {
	if err := validateVerdictForPublish(verdict); err != nil {
		return "", err
	}
	if err := writeVerdictState(root, repo, selectedNumber, verdict); err != nil {
		return "", err
	}
	return renderScopeGateStateComment(renderVerdictComment(verdict), scopeGateParked), nil
}

// writeVerdictState is the record's authoritative write, called BEFORE the
// caller posts the human-visible comment projection. Appends a journal audit
// event on success via the stage-pod-safe annotation seam (#3898) — the same
// mechanism failurestreakstate.go uses, which already works identically
// whether this stage is running in the daemon's own process, on a
// type-1/type-2 shared host, or in a pod.
func writeVerdictState(root string, repo providers.RepositoryRef, prNumber int, verdict apiv1.Verdict) error {
	l := instance.NewLayout(root)
	store, err := openStageStateStore(l)
	if err != nil {
		return fmt.Errorf("open remediation-verdict state: %w", err)
	}
	key := verdictRecordKey(repo, prNumber)
	if err := updateVerdictRecord(stateContext(), store, key, func(apiv1.Verdict, bool) (apiv1.Verdict, bool, error) {
		return verdict, true, nil
	}); err != nil {
		return fmt.Errorf("persist remediation-verdict state for PR #%d: %w", prNumber, err)
	}
	if annotations, aerr := openStageAnnotator(l); aerr == nil {
		_ = annotations.Append(journal.Event{
			Type: journal.EventRunnerAnnotation,
			Runner: map[string]any{
				"annotation": "remediation-verdict",
				"key":        key,
				"decision":   string(verdict.Decision),
			},
		})
		_ = annotations.Close()
	}
	return nil
}
