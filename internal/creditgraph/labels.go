package creditgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

const (
	// LabelStoreSchemaVersion identifies the persisted ground-truth label store.
	LabelStoreSchemaVersion = "goobers.dev/backprop/labels/v1"
	// LabelStoreFileName is the run-scoped ground-truth label store kept
	// beside attribution.json. It is append-only: recording a label never
	// rewrites the journal, the attribution record, or an earlier label.
	LabelStoreFileName = "labels.json"
	// maxLabelReasonBytes bounds operator free text kept in the store.
	maxLabelReasonBytes = 2048
	// maxLabelsPerRun bounds the history kept for one run.
	maxLabelsPerRun = 256
)

// LabelOutcome is a ground-truth verdict about whether a run's result was
// correct, independent of the run's terminal phase.
type LabelOutcome string

const (
	// LabelCorrect means the labeler judged the run's result correct.
	LabelCorrect LabelOutcome = "correct"
	// LabelIncorrect means the labeler judged the run's result incorrect.
	LabelIncorrect LabelOutcome = "incorrect"
)

// LabelSource names the kind of signal that produced a ground-truth label.
type LabelSource string

const (
	// LabelSourceHuman is an explicit operator verdict recorded via the CLI.
	LabelSourceHuman LabelSource = "human"
)

// GroundTruthLabel is one provenance-carrying ground-truth verdict for a run.
// Labels are read-only inputs to attribution aggregates: they never change a
// run's phase, gate verdicts, journal, or attribution record.
type GroundTruthLabel struct {
	ID         string       `json:"id"`
	RunID      string       `json:"runId"`
	Outcome    LabelOutcome `json:"outcome"`
	Reason     string       `json:"reason,omitempty"`
	Source     LabelSource  `json:"source"`
	LabeledBy  string       `json:"labeledBy"`
	LabeledAt  time.Time    `json:"labeledAt"`
	RecordedAt time.Time    `json:"recordedAt"`
}

// LabelStore is the persisted label history for one run.
type LabelStore struct {
	Schema string             `json:"schema"`
	RunID  string             `json:"runId"`
	Labels []GroundTruthLabel `json:"labels"`
}

// ParseLabelOutcome validates an operator-supplied outcome.
func ParseLabelOutcome(raw string) (LabelOutcome, error) {
	switch outcome := LabelOutcome(strings.TrimSpace(raw)); outcome {
	case LabelCorrect, LabelIncorrect:
		return outcome, nil
	default:
		return "", fmt.Errorf("outcome must be %q or %q", LabelCorrect, LabelIncorrect)
	}
}

func (label GroundTruthLabel) validate() error {
	if strings.TrimSpace(label.RunID) == "" {
		return errors.New("label run ID is required")
	}
	if _, err := ParseLabelOutcome(string(label.Outcome)); err != nil {
		return err
	}
	if label.Source != LabelSourceHuman {
		return fmt.Errorf("unsupported label source %q", label.Source)
	}
	if strings.TrimSpace(label.LabeledBy) == "" {
		return errors.New("label provenance requires who recorded it")
	}
	if label.LabeledAt.IsZero() || label.RecordedAt.IsZero() {
		return errors.New("label provenance requires labeled and recorded times")
	}
	if len(label.Reason) > maxLabelReasonBytes {
		return fmt.Errorf("label reason exceeds %d bytes", maxLabelReasonBytes)
	}
	return nil
}

func labelID(label GroundTruthLabel) string {
	label.ID = ""
	label.RecordedAt = time.Time{}
	data, _ := json.Marshal(label)
	sum := sha256.Sum256(data)
	return "label-" + hex.EncodeToString(sum[:10])
}

// RecordLabel appends one ground-truth label to the run's label store. The
// run must already carry a published attribution record, so only enrolled
// terminal runs can be labeled. A label whose verdict and provenance match an
// existing one is recorded once.
func RecordLabel(ctx context.Context, runDir string, label GroundTruthLabel) (GroundTruthLabel, error) {
	label.RunID = strings.TrimSpace(label.RunID)
	label.LabeledBy = strings.TrimSpace(label.LabeledBy)
	label.Reason = strings.TrimSpace(label.Reason)
	label.LabeledAt = label.LabeledAt.UTC()
	label.RecordedAt = label.RecordedAt.UTC()
	if err := label.validate(); err != nil {
		return GroundTruthLabel{}, fmt.Errorf("record label: %w", err)
	}
	record, err := ReadRunRecord(runDir)
	if errors.Is(err, os.ErrNotExist) {
		return GroundTruthLabel{}, fmt.Errorf("record label: run %q has no Backprop attribution record (not enrolled or not terminal)", label.RunID)
	}
	if err != nil {
		return GroundTruthLabel{}, fmt.Errorf("record label: %w", err)
	}
	if record.RunID != label.RunID {
		return GroundTruthLabel{}, fmt.Errorf("record label: attribution record belongs to run %q, not %q", record.RunID, label.RunID)
	}
	label.ID = labelID(label)
	stored := label
	err = withLabelStoreLock(ctx, runDir, func() error {
		store, readErr := ReadLabelStore(runDir)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		if errors.Is(readErr, os.ErrNotExist) {
			store = LabelStore{Schema: LabelStoreSchemaVersion, RunID: label.RunID}
		}
		if store.RunID != label.RunID {
			return fmt.Errorf("label store belongs to run %q, not %q", store.RunID, label.RunID)
		}
		for _, existing := range store.Labels {
			if existing.ID == label.ID {
				stored = existing
				return nil
			}
		}
		if len(store.Labels) >= maxLabelsPerRun {
			return fmt.Errorf("run %q already has the maximum of %d labels", label.RunID, maxLabelsPerRun)
		}
		store.Labels = append(store.Labels, label)
		data, marshalErr := json.MarshalIndent(store, "", "  ")
		if marshalErr != nil {
			return fmt.Errorf("encode label store: %w", marshalErr)
		}
		data = append(data, '\n')
		return journal.WriteFileAtomic(filepath.Join(runDir, LabelStoreFileName), data, 0o600)
	})
	if err != nil {
		return GroundTruthLabel{}, fmt.Errorf("record label: %w", err)
	}
	return stored, nil
}

func withLabelStoreLock(ctx context.Context, runDir string, fn func() error) (err error) {
	lockPath := filepath.Join(runDir, LabelStoreFileName+".lock")
	var held *platformlock.Handle
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		var acquireErr error
		held, acquireErr = platformlock.TryAcquire(lockPath)
		if acquireErr == nil {
			break
		}
		if !errors.Is(acquireErr, platformlock.ErrHeld) {
			return fmt.Errorf("lock label store: %w", acquireErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer func() {
		if releaseErr := held.Release(); err == nil && releaseErr != nil {
			err = fmt.Errorf("unlock label store: %w", releaseErr)
		}
	}()
	return fn()
}

// ReadLabelStore reads a run's ground-truth label history. A run with no
// labels returns an error wrapping os.ErrNotExist.
func ReadLabelStore(runDir string) (LabelStore, error) {
	data, err := os.ReadFile(filepath.Join(runDir, LabelStoreFileName))
	if err != nil {
		return LabelStore{}, err
	}
	var store LabelStore
	if err := json.Unmarshal(data, &store); err != nil {
		return LabelStore{}, fmt.Errorf("decode label store: %w", err)
	}
	if store.Schema != LabelStoreSchemaVersion {
		return LabelStore{}, fmt.Errorf("unsupported label store schema %q", store.Schema)
	}
	return store, nil
}

// EffectiveLabel resolves a label history to the single verdict aggregates
// use: the label with the latest LabeledAt, ties broken by ID. Resolution is
// independent of the order labels were recorded in, so a late-arriving label
// re-scores aggregates deterministically.
func EffectiveLabel(labels []GroundTruthLabel) (GroundTruthLabel, bool) {
	if len(labels) == 0 {
		return GroundTruthLabel{}, false
	}
	sorted := append([]GroundTruthLabel(nil), labels...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].LabeledAt.Equal(sorted[j].LabeledAt) {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].LabeledAt.Before(sorted[j].LabeledAt)
	})
	return sorted[len(sorted)-1], true
}

// Ground-truth weighting. A correct verdict confirms a run's journal-derived
// attribution and an incorrect verdict contradicts it, so labeled runs move
// cohort averages and audit confidence relative to unlabeled runs, which keep
// their unweighted values.
const (
	groundTruthCorrectWeight   = 2.0
	groundTruthUnlabeledWeight = 1.0
	groundTruthIncorrectWeight = 0.5
)

// groundTruthWeight is a run's weight in cohort and fault-audit averages.
func groundTruthWeight(label *GroundTruthLabel) float64 {
	if label == nil {
		return groundTruthUnlabeledWeight
	}
	switch label.Outcome {
	case LabelCorrect:
		return groundTruthCorrectWeight
	case LabelIncorrect:
		return groundTruthIncorrectWeight
	default:
		return groundTruthUnlabeledWeight
	}
}

// groundTruthConfidence calibrates one run's confidence by its verdict: a
// correct label halves the remaining uncertainty and an incorrect label halves
// the confidence.
func groundTruthConfidence(confidence float64, label *GroundTruthLabel) float64 {
	if label == nil {
		return confidence
	}
	switch label.Outcome {
	case LabelCorrect:
		return confidence + (1-confidence)/2
	case LabelIncorrect:
		return confidence / 2
	default:
		return confidence
	}
}
