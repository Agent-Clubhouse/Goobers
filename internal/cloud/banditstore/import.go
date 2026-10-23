package banditstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"

	"github.com/goobers/goobers/internal/bandit"
	"github.com/goobers/goobers/internal/journal"
)

// ImportResult counts committed observations and verified exact retries. On
// error these counts describe the completed prefix; re-running resumes safely.
type ImportResult struct{ Imported, Existing int }

// ImportLocalJournals is the cutover tool's one-time journal observation import.
// It reads source journals without migration or writes, and fences each bounded
// database transaction with this Store's acquired lease. The source must be the
// drained cutover snapshot. Partial imports are safe to repeat after takeover.
func (s *Store) ImportLocalJournals(ctx context.Context, runsDir string) (ImportResult, error) {
	var result ImportResult
	if err := s.manager.WithFence(ctx, s.handle, func(context.Context, pgx.Tx) error { return nil }); err != nil {
		return result, err
	}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !entry.IsDir() {
			continue
		}
		reader, err := journal.OpenReadOnly(filepath.Join(runsDir, entry.Name()))
		if errors.Is(err, journal.ErrNotRunDirectory) {
			continue
		}
		if err != nil {
			return result, err
		}
		if err := s.importJournal(ctx, reader, &result); err != nil {
			return result, fmt.Errorf("import journal %q: %w", entry.Name(), err)
		}
	}
	return result, nil
}

func (s *Store) importJournal(ctx context.Context, reader *journal.Reader, result *ImportResult) error {
	identity, err := reader.Identity()
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Type != journal.EventBanditObservation {
			continue
		}
		if event.Seq == 0 || event.Seq > math.MaxInt64 {
			return errors.New("bandit import requires a durable journal sequence")
		}
		data, err := json.Marshal(event.Outputs)
		if err != nil {
			return err
		}
		var observation bandit.Observation
		if err := json.Unmarshal(data, &observation); err != nil {
			return err
		}
		record := bandit.ObservationRecord{
			Key:         bandit.ObservationKey{RunID: identity.RunID, Stage: event.Stage, Branch: event.Branch, Occurrence: int64(event.Seq), Attempt: 1},
			Observation: observation,
		}
		_, payload, err := bandit.EncodeRecord(record)
		if err != nil {
			return err
		}
		inserted, err := s.put(ctx, journalObservationKey(identity.RunID, event.Seq), event.Stage, payload)
		if err != nil {
			return err
		}
		if inserted {
			result.Imported++
		} else {
			result.Existing++
		}
	}
	return nil
}

func journalObservationKey(runID string, sequence uint64) string {
	// A namespace distinct from native attempt keys prevents collisions even
	// when a legacy sequence equals a new workflow's occurrence ordinal.
	identity := fmt.Sprintf("%s\x00%d", runID, sequence)
	return fmt.Sprintf("journal:%x", sha256.Sum256([]byte(identity)))
}
