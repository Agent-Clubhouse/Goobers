package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/internal/workbenchservice"
	"github.com/goobers/goobers/providers"
)

// workbenchLearnedBlocks shares the scheduler's exact claims.lock. The callback
// owns this lock until its provider work and detached receipt commit have joined.
// No stateclient.Update retry or SQLite transaction spans provider effects.
func workbenchLearnedBlocks(schedulerDirectory string) workbenchservice.LearnedBlockScope {
	return func(ctx context.Context, repo providers.RepositoryRef, id string, use func(context.Context, workbenchservice.LearnedBlock) error) error {
		if use == nil || !filepath.IsAbs(schedulerDirectory) || blockedRepositoryEmpty(repo) {
			return errors.New("invalid resolution block custody")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		timeout := 5 * time.Second
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < timeout {
			timeout = time.Until(deadline)
		}
		if timeout <= 0 {
			return context.DeadlineExceeded
		}
		held, err := acquireClaimLock(filepath.Join(schedulerDirectory, "claims.lock"), "resolve-needs-human", timeout, time.Now())
		if err != nil {
			return err
		}
		defer func() { _ = held.Release() }()
		if err = ctx.Err(); err != nil {
			return err
		}
		records, err := resolutionBlockedRecords(schedulerDirectory)
		if err != nil {
			return err
		}
		learned, err := resolutionLearnedTarget(records, repo, id)
		if err != nil {
			return err
		}
		return use(ctx, learned)
	}
}
func resolutionBlockedRecords(directory string) (map[string]blockedRecord, error) {
	file, err := os.Open(filepath.Join(directory, stateclient.KeyBlockedRecords))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]blockedRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, stateclient.MaxValueBytes+1))
	if err != nil || len(raw) > stateclient.MaxValueBytes {
		return nil, errors.New("resolution block inventory exceeds bounds")
	}
	return decodeBlockedRecords(stateclient.Value{Data: raw, ETag: stateclient.ETagFor(raw)})
}
func resolutionLearnedTarget(records map[string]blockedRecord, repo providers.RepositoryRef, id string) (workbenchservice.LearnedBlock, error) {
	learned := workbenchservice.LearnedBlock{Complete: true}
	var selected *blockedRecord
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		record := records[key]
		if blockedLookupID(blockedRecordItemID(key, record)) != id {
			continue
		}
		if blockedRepositoryEmpty(record.Repository) {
			learned.Complete = false
			continue
		}
		if !resolutionSameRepository(record.Repository, repo) {
			continue
		}
		if selected != nil || blockedRecordItemID(key, record) != id {
			learned.Complete = false
		}
		copy := record
		selected = &copy
	}
	raw, err := json.Marshal(struct {
		Repository providers.RepositoryRef
		ID         string
		Record     *blockedRecord
		Complete   bool
	}{repo, id, selected, learned.Complete})
	if err != nil {
		return learned, err
	}
	digest := sha256.Sum256(raw)
	learned.Digest = hex.EncodeToString(digest[:])
	if selected != nil {
		learned.Reason = selected.Reason
		learned.Blockers = append([]string(nil), selected.Blockers...)
		if len(learned.Reason) > 8192 {
			learned.Reason = ""
			learned.Complete = false
		}
		if len(learned.Blockers) > providers.MaxAttentionBlockers {
			learned.Blockers = learned.Blockers[:providers.MaxAttentionBlockers]
			learned.Complete = false
		}
	}
	return learned, nil
}

func resolutionSameRepository(a, b providers.RepositoryRef) bool {
	return a.Provider == b.Provider && strings.EqualFold(a.Owner, b.Owner) && strings.EqualFold(a.Project, b.Project) && strings.EqualFold(a.Name, b.Name)
}
