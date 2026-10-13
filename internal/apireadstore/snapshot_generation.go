package apireadstore

import (
	"crypto/rand"
	"database/sql"
	"errors"
)

const snapshotGenerationSchema = `
CREATE TABLE snapshot_generations (snapshot TEXT PRIMARY KEY, generation TEXT NOT NULL, sequence INTEGER NOT NULL);
CREATE INDEX snapshot_generations_eviction ON snapshot_generations(sequence);
`

// SnapshotGeneration returns a shared random identity for the current snapshot.
// Eviction is safe: recreating an evicted row generates a new identity, so an old
// in-flight fill can never become the active generation again.
func (s *Store) SnapshotGeneration(snapshot string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var generation string
	err = tx.QueryRow(`SELECT generation FROM snapshot_generations WHERE snapshot=?`, snapshot).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		generation, err = s.rotateSnapshotGeneration(tx, snapshot)
	}
	if err != nil {
		return "", err
	}
	return generation, tx.Commit()
}

func (s *Store) rotateSnapshotGeneration(tx *sql.Tx, snapshot string) (string, error) {
	generation := rand.Text()
	_, err := tx.Exec(`INSERT INTO snapshot_generations(snapshot,generation,sequence)
VALUES(?,?,(SELECT coalesce(max(sequence),0)+1 FROM snapshot_generations))
ON CONFLICT(snapshot) DO UPDATE SET generation=excluded.generation,sequence=excluded.sequence`, snapshot, generation)
	if err != nil {
		return "", err
	}
	// Bound generation metadata independently of response bytes and rows.
	_, err = tx.Exec(`DELETE FROM snapshot_generations WHERE snapshot IN
(SELECT snapshot FROM snapshot_generations ORDER BY sequence DESC,snapshot LIMIT -1 OFFSET ?)`, max(1, s.maxEntries))
	return generation, err
}

// InvalidateSnapshot atomically rotates its generation and removes current
// aliases. A late response can only fill an old, unreachable generation. Other
// snapshots keep their generations and bodies; conditional base entries remain.
func (s *Store) InvalidateSnapshot(snapshot string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = s.rotateSnapshotGeneration(tx, snapshot); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM entries WHERE snapshot=?`, snapshot); err != nil {
		return err
	}
	return tx.Commit()
}
