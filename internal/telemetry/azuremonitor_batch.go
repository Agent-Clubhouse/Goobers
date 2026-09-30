package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"time"
)

// Leases survive process death; an interrupted upload becomes eligible again.
// Production HTTP attempts have a five-second deadline, shorter than this lease.
const azureReplayLease = 30 * time.Second

func selectReplayFiles(ctx context.Context, tx *sql.Tx, where string, args ...any) ([]indexedReplayFile, error) {
	rows, err := tx.QueryContext(ctx, `SELECT stream,name,bytes,modified,created,records FROM files WHERE `+where+` ORDER BY created,name LIMIT 128`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var files []indexedReplayFile
	for rows.Next() {
		var f indexedReplayFile
		if err = rows.Scan(&f.stream, &f.name, &f.bytes, &f.modified, &f.created, &f.records); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

func (s *azureReplaySpool) enforceIndexedBounds(ctx context.Context, tx *sql.Tx, now time.Time) error {
	for {
		files, err := selectReplayFiles(ctx, tx, `created < ? AND lease_until <= ?`, now.Add(-s.cfg.maxAge).UnixNano(), time.Now().UnixNano())
		if err != nil {
			return err
		}
		if len(files) == 0 {
			break
		}
		for _, f := range files {
			if err = s.index.remove(ctx, tx, f); err != nil {
				return err
			}
			if f.created == 0 {
				s.malformed.Add(1)
			} else {
				s.prunedAge.Add(uint64(f.records))
			}
		}
	}
	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM totals`).Scan(&total); err != nil {
		return err
	}
	for total > s.cfg.maxBytes {
		files, err := selectReplayFiles(ctx, tx, `lease_until <= ?`, time.Now().UnixNano())
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return errors.New("azure monitor replay capacity occupied by in-flight batches")
		}
		for _, f := range files {
			if total <= s.cfg.maxBytes {
				break
			}
			if err = s.index.remove(ctx, tx, f); err != nil {
				return err
			}
			s.prunedBytes.Add(uint64(f.records))
			total -= f.bytes
		}
	}
	return nil
}

type azureReplayBatch struct {
	files   []indexedReplayFile
	payload []byte
	owner   string
}

func (s *azureReplaySpool) claimBatch(ctx context.Context) (azureReplayBatch, error) {
	batch := azureReplayBatch{}
	owner, err := azureReplayID()
	if err != nil {
		return batch, err
	}
	batch.owner = owner
	err = s.index.withLock(ctx, func(tx *sql.Tx) error {
		files, err := selectReplayFiles(ctx, tx, `stream=? AND lease_until <= ?`, s.stream, time.Now().UnixNano())
		if err != nil {
			return err
		}
		if len(files) == 0 {
			var remaining int
			if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(files),0) FROM totals WHERE stream=?`, s.stream).Scan(&remaining); err != nil {
				return err
			}
			if remaining > 0 {
				return errors.New("azure monitor replay batches are leased")
			}
			return nil
		}
		count := 0
		for _, f := range files {
			if len(batch.files) > 0 && (count+f.records > azureReplayBatchRecords || int64(len(batch.payload))+f.bytes > azureReplayBatchBytes) {
				break
			}
			path, err := s.index.path(f)
			if err != nil {
				return err
			}
			_, payload, readErr := readAzureReplayFile(path)
			if readErr != nil {
				if !errors.Is(readErr, os.ErrNotExist) {
					s.malformed.Add(1)
				}
				if err = s.index.remove(ctx, tx, f); err != nil {
					return err
				}
				continue
			}
			batch.files = append(batch.files, f)
			batch.payload = append(batch.payload, payload...)
			if len(batch.payload) > 0 && batch.payload[len(batch.payload)-1] != '\n' {
				batch.payload = append(batch.payload, '\n')
			}
			count += azureReplayRecordCount(payload)
			if count >= azureReplayBatchRecords || len(batch.payload) >= azureReplayBatchBytes {
				break
			}
		}
		if len(batch.files) == 0 {
			s.signal()
			return nil
		} // continue beyond an all-poison page
		args := []any{time.Now().Add(azureReplayLease).UnixNano(), owner, s.stream}
		for _, f := range batch.files {
			args = append(args, f.name)
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch.files)), ",")
		_, err = tx.ExecContext(ctx, `UPDATE files SET lease_until=?,lease_owner=? WHERE stream=? AND name IN (`+placeholders+`)`, args...)
		return err
	})
	return batch, err
}

func (s *azureReplaySpool) finishBatch(ctx context.Context, batch azureReplayBatch, delivered bool) error {
	return s.index.withLock(ctx, func(tx *sql.Tx) error {
		if !delivered {
			_, err := tx.ExecContext(ctx, `UPDATE files SET lease_until=0,lease_owner='' WHERE lease_owner=?`, batch.owner)
			return err
		}
		var owned int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE lease_owner=?`, batch.owner).Scan(&owned); err != nil {
			return err
		}
		if owned != len(batch.files) {
			return errors.New("azure monitor replay lease changed during upload")
		}
		for _, f := range batch.files {
			if err := ctx.Err(); err != nil {
				return err
			}
			path, err := s.index.path(f)
			if err != nil {
				return err
			}
			if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM files WHERE lease_owner=?`, batch.owner)
		return err
	})
}
