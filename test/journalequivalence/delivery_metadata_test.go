package journalequivalence

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// This is the complete equivalence exclusion list for engine delivery metadata.
// journalCommitOwner.assignEmitKeys (internal/engine/journalowner.go) assigns
// these idempotency keys; opEmitScope (internal/engine/emit.go) deliberately uses
// an empty scope for run-level events. They do not describe execution results.
// All other runner fields remain byte-for-byte comparison inputs.
var excludedDeliveryFields = []string{"runner.emitKey"}

// Explicitly review new runner fields when extending the corpus, even though
// they would be compared by default. Never grow the exclusion list implicitly.
var comparedRunnerFields = []string{
	// executorErrorEvent / local runner executor-error facts.
	"errorClass", "errorCode", "infraFailedAttemptCommittedWork", "retryFailureClass",
	// gate.recordVerdict / runJournal.gateEvaluated budget and decision facts.
	"repassAttempt", "gateAttempt", "escalated", "duplicateDiff", "verdictCacheHit", "diffDigest", "reason", "repassTarget",
	// routeRetryDecision and appendReviewerDiffRedaction annotations.
	"goober", "reviewed", "kind", "failureCode", "target", "redacted", "sourceDigest", "sourceBytes", "digest", "bytes",
}

func excludeDeliveryMetadata(event map[string]any) error {
	runner, ok := event["runner"].(map[string]any)
	if !ok {
		return nil
	}
	removed := false
	for field := range runner {
		if slices.Contains(excludedDeliveryFields, "runner."+field) {
			delete(runner, field)
			removed = true
		} else if !slices.Contains(comparedRunnerFields, field) {
			return fmt.Errorf("unreviewed equivalence field runner.%s; review its producer and compare it explicitly", field)
		}
	}
	if removed && len(runner) == 0 {
		delete(event, "runner")
	}
	return nil
}

func TestDeliveryMetadataExclusionIsExactAndEngineInternal(t *testing.T) {
	if len(excludedDeliveryFields) != 1 || excludedDeliveryFields[0] != "runner."+livejournal.EmitKeyRunnerField {
		t.Fatalf("unreviewed engine-internal exclusions: %v", excludedDeliveryFields)
	}
	const runID = "11111111111111111111111111111111"
	event := journal.Event{Runner: map[string]any{"emitKey": runID + "|0||0|1", "errorCode": "infra.failure"}, Outputs: map[string]any{"runner": map[string]any{"emitKey": "business-data"}}}
	var decoded map[string]any
	if err := json.Unmarshal(canonicalJSON(t, event, runID), &decoded); err != nil {
		t.Fatal(err)
	}
	runner := decoded["runner"].(map[string]any)
	if _, ok := runner["emitKey"]; ok || runner["errorCode"] != "infra.failure" {
		t.Fatalf("runner fields = %v", runner)
	}
	if decoded["outputs"].(map[string]any)["runner"].(map[string]any)["emitKey"] != "business-data" {
		t.Fatal("nested business field excluded")
	}
	for _, field := range []string{"newInternalField", "emitKeySuffix"} {
		if err := excludeDeliveryMetadata(map[string]any{"runner": map[string]any{field: true}}); err == nil {
			t.Fatalf("unreviewed runner.%s accepted", field)
		}
	}
}

// Run creation itself writes run.started before applying idempotent ops, so it
// has no emitKey. Every subsequently persisted cloud event must have one.
func validateCloudEmitKeys(events []journal.Event, runID string) error {
	seen := map[string]bool{}
	for i, event := range events {
		key, ok := event.Runner[livejournal.EmitKeyRunnerField].(string)
		if i == 0 && event.Type == journal.EventRunStarted && !ok {
			continue
		}
		parts := strings.Split(key, "|")
		if !ok || len(parts) != 5 || parts[0] != runID {
			return fmt.Errorf("event %d (%s): missing or malformed emitKey %q", i, event.Type, key)
		}
		if seen[key] {
			return fmt.Errorf("event %d: duplicate emitKey %q", i, key)
		}
		seen[key] = true
		for _, index := range []int{1, 3, 4} {
			number, err := strconv.Atoi(parts[index])
			if err != nil || number < 0 || strconv.Itoa(number) != parts[index] {
				return fmt.Errorf("event %d: malformed numeric emitKey segment %q", i, key)
			}
		}
		scope, attempt := event.Stage, event.Attempt
		if scope == "" {
			scope, attempt = event.Gate, 0
		}
		if parts[1] != strconv.Itoa(event.Branch) || parts[2] != scope || parts[3] != strconv.Itoa(attempt) {
			return fmt.Errorf("event %d: emitKey scope disagrees with event: %q", i, key)
		}
	}
	if len(seen) == 0 {
		return fmt.Errorf("no cloud emit keys observed")
	}
	return nil
}

func TestCloudEmitKeyAssertionRejectsMutations(t *testing.T) {
	const runID = "11111111111111111111111111111111"
	valid := journal.Event{Type: journal.EventRunFinished, Runner: map[string]any{"emitKey": runID + "|0||0|1"}}
	if err := validateCloudEmitKeys([]journal.Event{{Type: journal.EventRunStarted}, valid}, runID); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "other|0||0|1", runID + "|0|missing-name|0|1", runID + "|0||-1|1", runID + "|0||0|01", runID + "|0||0"} {
		if err := validateCloudEmitKeys([]journal.Event{{Type: journal.EventRunFinished, Runner: map[string]any{"emitKey": key}}}, runID); err == nil {
			t.Fatalf("accepted %q", key)
		}
	}
	if err := validateCloudEmitKeys([]journal.Event{valid, valid}, runID); err == nil {
		t.Fatal("accepted duplicate keys")
	}
	if err := validateCloudEmitKeys([]journal.Event{{Type: journal.EventRunFinished}}, runID); err == nil {
		t.Fatal("accepted missing key")
	}
}
