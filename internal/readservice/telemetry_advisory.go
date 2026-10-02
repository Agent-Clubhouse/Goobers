package readservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/readmodel"
)

const modelAssistedShadowSchema = "goobers.dev/backprop/model-assisted-shadow/v1"

// ModelAssistedShadowRecord is a persisted advisory answer and, when one
// becomes available, its later human or deterministic outcome.
type ModelAssistedShadowRecord struct {
	EvidenceDigest    string                            `json:"evidenceDigest"`
	Finding           *creditgraph.ModelAssistedFinding `json:"finding,omitempty"`
	RunID             string                            `json:"runId,omitempty"`
	NodeID            string                            `json:"nodeId"`
	Stage             string                            `json:"stage,omitempty"`
	Failure           string                            `json:"failure,omitempty"`
	OutcomeClass      creditgraph.FailureClass          `json:"outcomeClass,omitempty"`
	OutcomeSource     string                            `json:"outcomeSource,omitempty"`
	OutcomeProvenance string                            `json:"outcomeProvenance,omitempty"`
	MatchesOutcome    *bool                             `json:"matchesOutcome,omitempty"`
}

type modelAssistedShadowState struct {
	Schema  string                                `json:"schema"`
	Records map[string]*ModelAssistedShadowRecord `json:"records"`
}

func modelAssistedShadowPath(root string) string {
	return filepath.Join(instance.NewLayout(root).SchedulerDir(), "backprop-audit", "model-assisted-shadow.json")
}

// StoredModelAssistedShadow records and replays shadow classifications for
// stored unknown causes. Existing records are joined to later deterministic
// classifications of the same run node before any new model calls are made.
func StoredModelAssistedShadow(
	ctx context.Context,
	root string,
	reads readmodel.Reader,
	query StoredAttributionQuery,
	classifier creditgraph.AdvisoryClassifier,
) error {
	if classifier.Gate == nil || strings.TrimSpace(root) == "" || reads == nil {
		return nil
	}
	observations, err := storedAttributionObservations(ctx, root, reads, query)
	if err != nil {
		return err
	}
	return updateModelAssistedShadow(ctx, root, func(state *modelAssistedShadowState) error {
		compareModelAssistedOutcomes(state, observations)
		for _, observation := range observations {
			attribution := observation.Attribution
			for _, cause := range attribution.Causes {
				if cause.Class != creditgraph.ClassUnknown {
					continue
				}
				digest, digestErr := creditgraph.ModelAssistedEvidenceDigest(classifier, attribution, cause)
				if digestErr != nil {
					digest = failedAdvisoryDigest(observation.RunID, cause)
				}
				if _, recorded := state.Records[digest]; recorded {
					continue
				}
				record := &ModelAssistedShadowRecord{
					EvidenceDigest: digest, RunID: observation.RunID,
					NodeID: cause.NodeID, Stage: cause.Stage,
				}
				if digestErr != nil {
					record.Failure = digestErr.Error()
					state.Records[digest] = record
					continue
				}
				one := attribution
				one.Causes = []creditgraph.CauseFinding{cause}
				findings, classifyErr := creditgraph.ClassifyUnknownShadow(ctx, classifier, one)
				if classifyErr != nil {
					record.Failure = classifyErr.Error()
				} else if len(findings) != 1 {
					record.Failure = fmt.Sprintf("creditgraph: expected one model-assisted finding, got %d", len(findings))
				} else {
					record.Finding = &findings[0]
				}
				state.Records[digest] = record
			}
		}
		return nil
	})
}

func compareModelAssistedOutcomes(state *modelAssistedShadowState, observations []creditgraph.AttributionObservation) {
	for _, observation := range observations {
		for _, cause := range observation.Attribution.Causes {
			if cause.Class == creditgraph.ClassUnknown {
				continue
			}
			for _, record := range state.Records {
				if record.RunID != observation.RunID || record.NodeID != cause.NodeID || record.Finding == nil {
					continue
				}
				if record.OutcomeSource == "human" {
					continue
				}
				matches := record.Finding.SuggestedClass == cause.Class
				record.OutcomeClass = cause.Class
				record.OutcomeSource = "deterministic-rule"
				record.OutcomeProvenance = ""
				record.MatchesOutcome = &matches
			}
		}
	}
}

// RecordModelAssistedHumanOutcome joins an explicit human classification to
// existing shadow suggestions. provenance identifies the recorded review.
func RecordModelAssistedHumanOutcome(
	ctx context.Context,
	root, runID, nodeID string,
	class creditgraph.FailureClass,
	provenance string,
) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("record model-assisted human outcome: root is required")
	}
	if strings.TrimSpace(runID) == "" {
		return errors.New("record model-assisted human outcome: run ID is required")
	}
	if strings.TrimSpace(nodeID) == "" {
		return errors.New("record model-assisted human outcome: node ID is required")
	}
	if !knownFailureClass(class) || class == creditgraph.ClassUnknown {
		return fmt.Errorf("record model-assisted human outcome: invalid outcome class %q", class)
	}
	provenance = strings.TrimSpace(provenance)
	if provenance == "" {
		return errors.New("record model-assisted human outcome: provenance is required")
	}
	return updateModelAssistedShadow(ctx, root, func(state *modelAssistedShadowState) error {
		matched := false
		for _, record := range state.Records {
			if record.RunID != runID || record.NodeID != nodeID || record.Finding == nil {
				continue
			}
			matches := record.Finding.SuggestedClass == class
			record.OutcomeClass = class
			record.OutcomeSource = "human"
			record.OutcomeProvenance = provenance
			record.MatchesOutcome = &matches
			matched = true
		}
		if !matched {
			return fmt.Errorf("record model-assisted human outcome: no shadow suggestion for run %q node %q", runID, nodeID)
		}
		return nil
	})
}

func knownFailureClass(class creditgraph.FailureClass) bool {
	switch class {
	case creditgraph.ClassBadToolChoice,
		creditgraph.ClassBadToolResult,
		creditgraph.ClassBadInterpretation,
		creditgraph.ClassWeakInstructions,
		creditgraph.ClassRouting,
		creditgraph.ClassModel,
		creditgraph.ClassTopology,
		creditgraph.ClassEnvironment,
		creditgraph.ClassUnknown:
		return true
	default:
		return false
	}
}

func failedAdvisoryDigest(runID string, cause creditgraph.CauseFinding) string {
	return "invalid:" + runID + ":" + cause.NodeID
}

// ReadModelAssistedShadow returns the persisted shadow records for offline
// comparison. It never invokes the model.
func ReadModelAssistedShadow(root string) ([]ModelAssistedShadowRecord, error) {
	state, err := readModelAssistedShadowState(root)
	if err != nil {
		return nil, err
	}
	records := make([]ModelAssistedShadowRecord, 0, len(state.Records))
	for _, record := range state.Records {
		records = append(records, *record)
	}
	return records, nil
}

func readModelAssistedShadowState(root string) (modelAssistedShadowState, error) {
	state := modelAssistedShadowState{
		Schema:  modelAssistedShadowSchema,
		Records: map[string]*ModelAssistedShadowRecord{},
	}
	data, err := os.ReadFile(modelAssistedShadowPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return modelAssistedShadowState{}, fmt.Errorf("read model-assisted shadow state: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return modelAssistedShadowState{}, fmt.Errorf("decode model-assisted shadow state: %w", err)
	}
	if state.Schema != modelAssistedShadowSchema {
		return modelAssistedShadowState{}, fmt.Errorf("decode model-assisted shadow state: unsupported schema %q", state.Schema)
	}
	if state.Records == nil {
		return modelAssistedShadowState{}, errors.New("decode model-assisted shadow state: records are required")
	}
	for digest, record := range state.Records {
		if record == nil || record.EvidenceDigest != digest || record.NodeID == "" {
			return modelAssistedShadowState{}, fmt.Errorf("decode model-assisted shadow state: invalid record %q", digest)
		}
	}
	return state, nil
}

func updateModelAssistedShadow(
	ctx context.Context,
	root string,
	update func(*modelAssistedShadowState) error,
) (err error) {
	path := modelAssistedShadowPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create model-assisted shadow directory: %w", err)
	}
	var held *platformlock.Handle
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		held, err = platformlock.TryAcquire(path + ".lock")
		if err == nil {
			break
		}
		if !errors.Is(err, platformlock.ErrHeld) {
			return fmt.Errorf("lock model-assisted shadow state: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer func() {
		if releaseErr := held.Release(); err == nil && releaseErr != nil {
			err = fmt.Errorf("unlock model-assisted shadow state: %w", releaseErr)
		}
	}()
	state, err := readModelAssistedShadowState(root)
	if err != nil {
		return err
	}
	if err := update(&state); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode model-assisted shadow state: %w", err)
	}
	if err := journal.WriteFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write model-assisted shadow state: %w", err)
	}
	return nil
}
