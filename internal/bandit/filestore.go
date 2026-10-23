package bandit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/platform/durability"
)

// FileStore stores immutable observation records beneath an operator-owned root.
// Atomic no-clobber publication arbitrates concurrent processes and retries.
type FileStore struct{ root string }

// NewFileStore opens a local observation store without accepting request paths.
func NewFileStore(root string) (*FileStore, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("bandit: absolute observation-store root required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &FileStore{root: root}, nil
}

func (s *FileStore) stageDirectory(stage string) string {
	return filepath.Join(s.root, fmt.Sprintf("%x", sha256.Sum256([]byte(stage))))
}

// Record commits a new attempt outcome or verifies an exact retry.
func (s *FileStore) Record(ctx context.Context, record ObservationRecord) error {
	key, payload, err := EncodeRecord(record)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := s.stageDirectory(record.Key.Stage)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var conflict error
	err = durability.WriteFileAtomic(filepath.Join(dir, key+".json"), payload, 0o600,
		durability.WithPublishRaceCheck(func(path string) error {
			existing, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Equal(existing, payload) {
				conflict = ErrObservationConflict
				return ErrObservationConflict
			}
			return nil
		}))
	return errors.Join(err, conflict)
}

// Observations returns a deterministically ordered snapshot of committed samples.
func (s *FileStore) Observations(ctx context.Context, stage string) ([]Observation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(stage) == "" || len(stage) > 256 {
		return nil, errors.New("bandit: invalid observation stage")
	}
	dir := s.stageDirectory(stage)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var observations []Observation
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue // Atomic publication's temporary files are not committed records.
		}
		payload, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var record ObservationRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, err
		}
		key, _, err := EncodeRecord(record)
		if err != nil || record.Key.Stage != stage || key+".json" != entry.Name() {
			return nil, errors.New("bandit: corrupt observation record")
		}
		observations = append(observations, record.Observation)
	}
	SortObservations(observations)
	return observations, nil
}
