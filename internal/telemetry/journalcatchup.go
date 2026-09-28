package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// The journal is the durable admission queue when Azure replay and an instance
// root are configured. Commit only offers a path hint; losing hints cannot lose
// records because paced discovery resumes every retained journal from a cursor.
type journalCatchup struct {
	pipeline                *journalLogPipeline
	root, spool, instanceID string
	maxAge                  time.Duration
	since                   time.Time
	hints                   chan journalCatchupHint
	discovered              chan string
	flushes                 chan journalLogFlush
	done                    chan struct{}
	cancel                  context.CancelFunc
	stopOnce                sync.Once
}

type journalCatchupHint struct {
	dir, identity string
	seq           uint64
}

func newJournalCatchup(cfg Config, pipeline *journalLogPipeline) *journalCatchup {
	ctx, cancel := context.WithCancel(context.Background())
	s := &journalCatchup{pipeline: pipeline, root: cfg.JournalRoot, spool: cfg.AzureMonitorReplayRoot,
		instanceID: cfg.JournalInstanceID, maxAge: cfg.AzureMonitorReplayMaxAge, since: time.Now(),
		hints: make(chan journalCatchupHint, 1024), discovered: make(chan string, 32), flushes: make(chan journalLogFlush),
		done: make(chan struct{}), cancel: cancel}
	go s.discover(ctx)
	go s.run(ctx)
	return s
}

func (s *journalCatchup) offer(hint journalCatchupHint) {
	select {
	case s.hints <- hint:
	default:
		s.pipeline.catchupDeferred.Add(1)
	}
}

func (s *journalCatchup) notify(event journal.CommittedEvent) {
	if len(event.JournalID) > 128 {
		return
	}
	hint := journalCatchupHint{identity: event.JournalID, seq: event.Seq}
	if event.Kind == "scheduler" {
		hint.dir = "scheduler"
		s.offer(hint)
	} else if event.Kind == "run" && apiv1.ValidRunID(event.RunID) {
		// Both layouts are supported; nonexistent candidates are cheap and never
		// create data. Reject separators before deriving a path from a label.
		if event.Gaggle != "" && len(event.Gaggle) <= 256 && event.Gaggle != "." && event.Gaggle != ".." && !strings.ContainsAny(event.Gaggle, `/\\`) {
			hint.dir = filepath.Join("gaggles", event.Gaggle, "runs", event.RunID)
			s.offer(hint)
		}
		hint.dir = filepath.Join("runs", event.RunID)
		s.offer(hint)
	}
}

func (s *journalCatchup) flush(ctx context.Context) error {
	request := journalLogFlush{ctx: ctx, done: make(chan error, 1)}
	select {
	case s.flushes <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return nil
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return nil
	}
}

func (s *journalCatchup) shutdown(ctx context.Context) {
	s.stopOnce.Do(func() {
		// Retained source records are already durable. A busy retry loop must
		// not spend the daemon's entire shutdown budget on a last-chance copy.
		flush, cancel := context.WithTimeout(ctx, journalLogTimeout)
		defer cancel()
		_ = s.flush(flush)
		s.cancel()
	})
	select {
	case <-s.done:
	case <-ctx.Done():
	}
}

func (s *journalCatchup) run(ctx context.Context) {
	defer close(s.done)
	var db *sql.DB
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	root, err := os.OpenRoot(s.root)
	if err != nil {
		s.failure()
		return
	}
	defer func() { _ = root.Close() }()
	lastPrune := time.Time{}
	for {
		if ctx.Err() != nil {
			return
		}
		if db == nil {
			var enrolled time.Time
			db, enrolled, err = openJournalCursorStore(ctx, s.spool, s.since)
			if err != nil {
				s.failure()
				if !s.waitForStoreRetry(ctx) {
					return
				}
				continue
			}
			s.since = enrolled
		}
		if time.Since(lastPrune) > time.Minute {
			if err = pruneJournalCursors(ctx, db, time.Now().Add(-s.maxAge)); err != nil {
				s.failure()
			}
			lastPrune = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case request := <-s.flushes:
			request.done <- s.flushHints(request.ctx, root, db)
		case hint := <-s.hints:
			// Collapse notification bursts to the latest committed sequence per
			// journal. Otherwise each event would force its own tiny disk batch.
			for _, pending := range s.coalesceHints(hint) {
				s.process(ctx, root, db, pending)
			}
		case dir := <-s.discovered:
			s.process(ctx, root, db, journalCatchupHint{dir: dir})
		}
	}
}

// A flush cannot make progress until the cursor store opens. Acknowledge that
// promptly instead of consuming the caller's entire shutdown deadline waiting
// for an initialization loop which cannot receive flush requests. The retained
// journals remain the source; requests must not accelerate the retry cadence.
func (s *journalCatchup) waitForStoreRetry(ctx context.Context) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case request := <-s.flushes:
			request.done <- errors.New("journal catch-up storage unavailable; retained records remain pending")
		}
	}
}

func (s *journalCatchup) coalesceHints(first journalCatchupHint) []journalCatchupHint {
	hints := []journalCatchupHint{first}
	positions := map[string]int{first.dir: 0}
	for range cap(s.hints) {
		select {
		case hint := <-s.hints:
			if i, ok := positions[hint.dir]; ok {
				if hints[i].identity != hint.identity || hint.seq > hints[i].seq {
					hints[i] = hint
				}
			} else {
				positions[hint.dir] = len(hints)
				hints = append(hints, hint)
			}
		default:
			return hints
		}
	}
	return hints
}

func (s *journalCatchup) failure() {
	s.pipeline.failures.Add(1)
	// Fixed, secret-free signature; ordinary pipeline reporting is rate limited.
	s.pipeline.reporter.Handle(errors.New("journal catch-up unavailable; retained journal records will be retried"))
}

func (s *journalCatchup) process(ctx context.Context, root *os.Root, db *sql.DB, hint journalCatchupHint) {
	attempt, cancel := context.WithTimeout(ctx, journalLogTimeout)
	defer cancel()
	more, err := s.processBatch(attempt, root, db, hint)
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, platformlock.ErrHeld) {
		s.failure()
	}
	if more || err != nil && !errors.Is(err, os.ErrNotExist) {
		// Preserve the last committed watermark after a transient storage error
		// or another process owning the cursor. Pace retries during an outage.
		if err == nil || waitJournalCatchup(ctx, time.Second) {
			s.offer(hint)
		}
	}
}

func (s *journalCatchup) processBatch(ctx context.Context, root *os.Root, db *sql.DB, hint journalCatchupHint) (bool, error) {
	dir := hint.dir
	cutoff := time.Now().Add(-s.maxAge)
	if s.since.After(cutoff) {
		cutoff = s.since
	}
	var runFile os.FileInfo
	if dir != "scheduler" {
		info, err := root.Stat(filepath.Join(dir, "events.jsonl"))
		if err != nil {
			return false, err
		}
		if info.ModTime().Before(cutoff) {
			return false, nil
		}
		runFile = info
	}
	lock, err := platformlock.TryAcquire(filepath.Join(s.spool, ".journal-cursors.lock"))
	if err != nil {
		return false, err
	}
	defer func() { _ = lock.Release() }()
	cursor, err := loadJournalCursor(ctx, db, dir)
	if err != nil {
		return false, err
	}
	if hint.seq > 0 && cursor.identity == hint.identity && cursor.seq >= hint.seq {
		return false, nil
	}
	fingerprint, unchanged, err := unchangedJournalRun(ctx, root, dir, runFile, cursor)
	if err != nil || unchanged {
		return false, err
	}
	batch, err := s.readBatch(ctx, root, hint, cursor)
	if err != nil {
		return false, err
	}
	if batch.Gap {
		s.pipeline.failures.Add(1)
		s.pipeline.reporter.Handle(errors.New("journal catch-up found a retained-history sequence gap; complete reconstruction is unavailable"))
	}
	before := s.pipeline.failures.Load()
	accepted := s.pipeline.accepted.Load()
	expected := uint64(0)
	for _, event := range batch.Events {
		if event.Time.Before(cutoff) {
			continue
		}
		if event.InstanceID == "" {
			event.InstanceID = s.instanceID
		}
		if s.pipeline.scrubber != nil {
			event.Body = s.pipeline.scrubber.Scrub(event.Body)
		}
		if journalLogSize(event) <= journalLogRecordLimit {
			expected++
		}
		s.pipeline.commit(event)
	}
	if err = s.pipeline.flush(ctx); err != nil {
		return false, err
	}
	if s.pipeline.failures.Load() != before || s.pipeline.accepted.Load()-accepted != expected {
		return false, errors.New("journal batch was not durably admitted")
	}
	next := journalCursor{identity: batch.Position.Identity, generation: batch.Position.Generation,
		offset: batch.Position.Offset, seq: batch.Position.Seq, fingerprint: fingerprint}
	if next != cursor {
		err = saveJournalCursor(ctx, db, dir, next)
	}
	return batch.More, err
}

func unchangedJournalRun(ctx context.Context, root *os.Root, dir string, info os.FileInfo, cursor journalCursor) (string, bool, error) {
	if info == nil {
		return "", false, nil
	} // Scheduler generation/identity must always be read.
	fingerprint, err := journal.ExportRunFingerprint(ctx, root, dir, info)
	unchanged := err == nil && cursor.identity == filepath.Base(dir) && cursor.offset == info.Size() && cursor.fingerprint == fingerprint
	return fingerprint, unchanged, err
}

func (s *journalCatchup) readBatch(ctx context.Context, root *os.Root, hint journalCatchupHint, cursor journalCursor) (journal.ExportBatch, error) {
	// Discovery only reads inactive journals while holding their writer lock.
	// Live hints cap reads at a sequence whose fsync already succeeded. Release
	// the journal lock BEFORE encoding, spool admission, cursor writes or HTTP.
	if hint.seq == 0 {
		lock, err := platformlock.TryAcquireExistingInRoot(root, filepath.Join(hint.dir, ".lock"))
		if err != nil {
			return journal.ExportBatch{}, err
		}
		defer func() { _ = lock.Release() }()
	}
	return journal.ReadExportBatch(ctx, root, hint.dir, hint.dir == "scheduler", journal.ExportPosition{
		Identity: cursor.identity, Generation: cursor.generation, Offset: cursor.offset, Seq: cursor.seq}, hint.seq)
}

func (s *journalCatchup) flushHints(ctx context.Context, root *os.Root, db *sql.DB) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case hint := <-s.hints:
			pendingHints := s.coalesceHints(hint)
			for i, pending := range pendingHints {
				for {
					more, err := s.processBatch(ctx, root, db, pending)
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						for _, retry := range pendingHints[i:] {
							s.offer(retry)
						}
						return err
					}
					if !more {
						break
					}
				}
			}
		default:
			return ctx.Err()
		}
	}
}

func waitJournalCatchup(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *journalCatchup) discover(ctx context.Context) {
	for {
		s.discoverOnce(ctx)
		if !waitJournalCatchup(ctx, time.Minute) {
			return
		}
	}
}

func (s *journalCatchup) discoverOnce(ctx context.Context) {
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	s.discoveryOffer(ctx, "scheduler")
	s.discoverRuns(ctx, root, "runs")
	_ = visitJournalDirectories(ctx, root, "gaggles", func(name string) {
		s.discoverRuns(ctx, root, filepath.Join("gaggles", name, "runs"))
	})
}

func (s *journalCatchup) discoveryOffer(ctx context.Context, dir string) {
	select {
	case s.discovered <- dir:
	case <-ctx.Done():
	}
}

func (s *journalCatchup) discoverRuns(ctx context.Context, root *os.Root, runs string) {
	_ = visitJournalDirectories(ctx, root, runs, func(name string) {
		if apiv1.ValidRunID(name) {
			s.discoveryOffer(ctx, filepath.Join(runs, name))
		}
	})
}

func visitJournalDirectories(ctx context.Context, root *os.Root, path string, visit func(string)) error {
	directory, err := root.OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	dir, err := directory.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	for {
		entries, err := dir.ReadDir(64)
		for _, entry := range entries {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if entry.IsDir() {
				visit(entry.Name())
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if !waitJournalCatchup(ctx, 100*time.Millisecond) {
			return ctx.Err()
		}
	}
}
