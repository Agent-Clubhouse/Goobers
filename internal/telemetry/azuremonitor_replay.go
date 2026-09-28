package telemetry

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	azureReplaySchema       = "goobers.dev/telemetry/azure-replay/v1"
	azureReplayFileSuffix   = ".ndjson"
	azureReplayHeaderLimit  = 4096
	azureReplayRetryMinimum = time.Second
	azureReplayRetryMaximum = time.Minute
	azureReplayDrainLimit   = 32
	azureReplayBatchRecords = 128
	azureReplayBatchBytes   = 1 << 20
)

var errAzureReplayYield = errors.New("azure monitor replay work budget exhausted")

type azureReplayConfig struct {
	root     string
	dir      string
	maxAge   time.Duration
	maxBytes int64
}

func (c Config) azureReplayConfig(stream string) azureReplayConfig {
	if c.AzureMonitorReplayRoot == "" {
		return azureReplayConfig{}
	}
	return azureReplayConfig{
		root:   c.AzureMonitorReplayRoot,
		dir:    filepath.Join(c.AzureMonitorReplayRoot, stream),
		maxAge: c.AzureMonitorReplayMaxAge, maxBytes: c.AzureMonitorReplayMaxBytes,
	}
}

type azureReplayHeader struct {
	Schema    string    `json:"schema"`
	CreatedAt time.Time `json:"createdAt"`
	Records   int       `json:"records,omitempty"` // Absent in older v1 spool files.
}

// AzureReplayStats is a low-cardinality snapshot of one signal spool.
type AzureReplayStats struct {
	Accepted, Delivered, Retried uint64
	PrunedAge, PrunedBytes       uint64
	Malformed                    uint64
	PendingRecords               int
	PendingBytes                 int64
	OldestPendingAge             time.Duration
	PendingFiles                 int
	AccountingReady              bool
	AdmissionFailures            uint64
	QueueDropped, ExportFailures uint64
}

var activeAzureReplaySpools = struct {
	sync.Mutex
	spools map[*azureReplaySpool]struct{}
}{spools: make(map[*azureReplaySpool]struct{})}

// InspectAzureReplayRoot returns aggregate pending state across the trace,
// journal, and diagnostic subspools without starting an exporter.
func InspectAzureReplayRoot(root string) AzureReplayStats {
	result, indexed := inspectReplayIndex(root)
	if !indexed {
		for _, stream := range []string{"traces", "journal", "diagnostics"} {
			dir := filepath.Join(root, stream)
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), azureReplayFileSuffix) {
					continue
				}
				info, err := entry.Info()
				if err != nil {
					continue
				}
				result.PendingBytes += info.Size()
				result.PendingFiles++
				createdAt, records, err := readAzureReplayMetadata(filepath.Join(dir, entry.Name()))
				if err == nil {
					result.PendingRecords += records
					age := time.Since(createdAt)
					if age > result.OldestPendingAge {
						result.OldestPendingAge = age
					}
				}
			}
		}
	}
	activeAzureReplaySpools.Lock()
	for spool := range activeAzureReplaySpools.spools {
		if filepath.Dir(spool.cfg.dir) != filepath.Clean(root) {
			continue
		}
		result.Accepted += spool.accepted.Load()
		result.Delivered += spool.delivered.Load()
		result.Retried += spool.retried.Load()
		result.PrunedAge += spool.prunedAge.Load()
		result.PrunedBytes += spool.prunedBytes.Load()
		result.Malformed += spool.malformed.Load()
		result.AdmissionFailures += spool.admissionFailures.Load()
		if source := spool.lossSource.Load(); source != nil {
			loss := source.sample()
			result.QueueDropped += loss.Dropped
			result.ExportFailures += loss.ExportFailures
		}
	}
	activeAzureReplaySpools.Unlock()
	return result
}

type azureReplaySpool struct {
	cfg        azureReplayConfig
	send       func(context.Context, []byte) error
	now        func() time.Time
	indexOnce  sync.Once
	index      *azureReplayIndex
	stream     string
	indexErr   error
	lastStats  atomic.Pointer[AzureReplayStats]
	lossSource atomic.Pointer[replayLossSource]
	healthDone chan struct{}
	wake       chan struct{}
	done       chan struct{}
	cancel     context.CancelFunc
	closed     atomic.Bool

	accepted, delivered, retried atomic.Uint64
	prunedAge, prunedBytes       atomic.Uint64
	malformed                    atomic.Uint64
	admissionFailures            atomic.Uint64
}

func (s *azureReplaySpool) ensureIndex() error {
	s.indexOnce.Do(func() { s.index, s.stream, s.indexErr = acquireReplayIndex(s.cfg) })
	return s.indexErr
}

func newAzureReplaySpool(cfg azureReplayConfig, send func(context.Context, []byte) error) (*azureReplaySpool, error) {
	if cfg.dir == "" {
		return nil, nil
	}
	if cfg.maxAge <= 0 || cfg.maxBytes <= 0 {
		return nil, errors.New("azure monitor replay requires positive age and byte bounds")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &azureReplaySpool{cfg: cfg, send: send, now: time.Now, wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel}
	if err := s.ensureIndex(); err != nil {
		cancel()
		return nil, err
	}
	activeAzureReplaySpools.Lock()
	activeAzureReplaySpools.spools[s] = struct{}{}
	activeAzureReplaySpools.Unlock()
	go s.run(ctx)
	s.healthDone = make(chan struct{})
	go s.runHealth(ctx)
	s.signal()
	return s, nil
}

func (s *azureReplaySpool) submit(ctx context.Context, payload []byte) (submitErr error) {
	if s == nil {
		return errors.New("azure monitor replay spool is unavailable")
	}
	defer func() {
		if submitErr != nil {
			s.admissionFailures.Add(uint64(azureReplayRecordCount(payload)))
		}
	}()
	if s.closed.Load() {
		return errors.New("azure monitor replay spool is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.now().UTC()
	id, err := azureReplayID()
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%020d-%s%s", now.UnixNano(), id, azureReplayFileSuffix)
	if err := s.ensureIndex(); err != nil {
		return err
	}
	var rejected bool
	err = s.index.withLock(ctx, func(tx *sql.Tx) error {
		if s.closed.Load() {
			return errors.New("azure monitor replay spool is closed")
		}
		path, err := s.writeLocked(name, now, payload)
		if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		f := indexedReplayFile{stream: s.stream, name: name, bytes: info.Size(), modified: info.ModTime().UnixNano(), created: now.UnixNano(), records: azureReplayRecordCount(payload)}
		if err = s.index.put(ctx, tx, f); err != nil {
			return err
		}
		if err = s.enforceIndexedBounds(ctx, tx, now); err != nil {
			return err
		}
		_, err = os.Stat(path)
		rejected = errors.Is(err, os.ErrNotExist)
		if rejected {
			return nil
		} // Commit the pruning/accounting, then report rejection.
		return err
	})
	if err != nil {
		return err
	}
	if rejected {
		return errors.New("azure monitor replay byte bound rejected the newest batch")
	}
	s.accepted.Add(uint64(azureReplayRecordCount(payload)))
	s.signal()
	// Acknowledges durable local admission, not remote ingestion. Only the
	// replay worker sends; producers cannot bypass its retry backoff.
	return nil
}

func (s *azureReplaySpool) writeLocked(name string, createdAt time.Time, payload []byte) (string, error) {
	tmp, err := os.CreateTemp(s.cfg.dir, ".pending-*")
	if err != nil {
		return "", fmt.Errorf("create Azure Monitor replay batch: %w", err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	header, err := json.Marshal(azureReplayHeader{Schema: azureReplaySchema, CreatedAt: createdAt, Records: azureReplayRecordCount(payload)})
	if err != nil {
		return "", fmt.Errorf("encode Azure Monitor replay header: %w", err)
	}
	if _, err := tmp.Write(append(header, '\n')); err != nil {
		return "", fmt.Errorf("write Azure Monitor replay header: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		return "", fmt.Errorf("write Azure Monitor replay payload: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("sync Azure Monitor replay batch: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close Azure Monitor replay batch: %w", err)
	}
	destination := filepath.Join(s.cfg.dir, name)
	if err := os.Rename(tmpName, destination); err != nil {
		return "", fmt.Errorf("publish Azure Monitor replay batch: %w", err)
	}
	keep = true
	return destination, nil
}

func (s *azureReplaySpool) drain(ctx context.Context) error {
	if err := s.ensureIndex(); err != nil {
		return err
	}
	if err := s.index.withLock(ctx, func(tx *sql.Tx) error { return s.enforceIndexedBounds(ctx, tx, s.now().UTC()) }); err != nil {
		return err
	}
	initialDelivered := s.delivered.Load()
	for range azureReplayDrainLimit {
		if err := ctx.Err(); err != nil {
			s.signal()
			return errors.Join(errAzureReplayYield, err)
		}
		batch, err := s.claimBatch(ctx)
		if err != nil {
			if ctx.Err() != nil && s.delivered.Load() > initialDelivered {
				s.signal()
				return errors.Join(errAzureReplayYield, err)
			}
			return err
		}
		if len(batch.files) == 0 {
			return nil
		}
		// The OS lock and database transaction are released before HTTP.
		attempt, cancelSend := context.WithTimeout(ctx, 5*time.Second)
		sendErr := s.send(attempt, batch.payload)
		cancelSend()
		// Cancellation must not strand a live process's claims for the entire lease.
		ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = s.finishBatch(ackCtx, batch, sendErr == nil)
		cancel()
		if sendErr != nil {
			s.retried.Add(1)
			return sendErr
		}
		if err != nil {
			return err
		}
		s.delivered.Add(uint64(azureReplayRecordCount(batch.payload)))
	}
	s.signal()
	return nil
}

func readAzureReplayFile(path string) (time.Time, []byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, nil, err
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReaderSize(file, azureReplayHeaderLimit)
	header, err := readAzureReplayHeader(reader)
	if err != nil {
		return time.Time{}, nil, err
	}
	payload, err := io.ReadAll(reader)
	if err != nil || len(payload) == 0 {
		return time.Time{}, nil, errors.New("invalid Azure Monitor replay payload")
	}
	for _, line := range bytesLines(payload) {
		if len(line) != 0 && !json.Valid(line) {
			return time.Time{}, nil, errors.New("invalid Azure Monitor replay envelope")
		}
	}
	if header.Records > 0 && header.Records != azureReplayRecordCount(payload) {
		return time.Time{}, nil, errors.New("invalid Azure Monitor replay record count")
	}
	return header.CreatedAt, payload, nil
}

func readAzureReplayHeader(reader *bufio.Reader) (azureReplayHeader, error) {
	// ReadSlice bounds allocation even if a damaged file has no newline.
	line, err := reader.ReadSlice('\n')
	var header azureReplayHeader
	if err != nil || json.Unmarshal(line, &header) != nil || header.Schema != azureReplaySchema || header.CreatedAt.IsZero() || header.Records < 0 {
		return header, errors.New("invalid Azure Monitor replay header")
	}
	return header, nil
}

// Reconciliation reads a small header; old v1 files without record counts need
// one full payload read. Routine bounds and health queries use the manifest.
// Payload validation still happens before each delivery.
func readAzureReplayMetadata(path string) (time.Time, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, 0, err
	}
	header, err := readAzureReplayHeader(bufio.NewReaderSize(file, azureReplayHeaderLimit))
	_ = file.Close()
	if err != nil {
		return time.Time{}, 0, err
	}
	if header.Records > 0 {
		return header.CreatedAt, header.Records, nil
	}
	createdAt, payload, err := readAzureReplayFile(path)
	return createdAt, azureReplayRecordCount(payload), err
}

func bytesLines(payload []byte) [][]byte {
	var lines [][]byte
	for len(payload) > 0 {
		index := 0
		for index < len(payload) && payload[index] != '\n' {
			index++
		}
		lines = append(lines, payload[:index])
		if index == len(payload) {
			break
		}
		payload = payload[index+1:]
	}
	return lines
}

func azureReplayRecordCount(payload []byte) int {
	count := 0
	for _, line := range bytesLines(payload) {
		if len(line) > 0 {
			count++
		}
	}
	return count
}

func (s *azureReplaySpool) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *azureReplaySpool) run(ctx context.Context) {
	defer close(s.done)
	delay := azureReplayRetryMinimum
	retrying := false
	for {
		var retry <-chan time.Time
		wake := s.wake
		var timer *time.Timer
		if retrying {
			// Leave new-work notifications pending until the backoff expires.
			// Continuous traffic must not turn an outage into a retry storm.
			wake = nil
			timer = time.NewTimer(jitterAzureReplayDelay(delay))
			retry = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-wake:
		case <-retry:
		}
		if timer != nil {
			timer.Stop()
		}
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.drain(attempt)
		cancel()
		if err == nil || errors.Is(err, errAzureReplayYield) {
			delay = azureReplayRetryMinimum
			retrying = false
			continue
		}
		retrying = true
		delay = min(delay*2, azureReplayRetryMaximum)
	}
}

func jitterAzureReplayDelay(maximum time.Duration) time.Duration {
	if maximum <= azureReplayRetryMinimum {
		return azureReplayRetryMinimum
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return maximum
	}
	var value uint64
	for _, b := range raw {
		value = value<<8 | uint64(b)
	}
	half := maximum / 2
	return half + time.Duration(value%uint64(maximum-half+1))
}

func azureReplayID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate Azure Monitor replay identity: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (s *azureReplaySpool) stats() AzureReplayStats {
	if s == nil {
		return AzureReplayStats{}
	}
	var stats AzureReplayStats
	if s.ensureIndex() == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		stats, _ = s.index.stats(ctx, s.stream, s.now())
		cancel()
	}
	if stats.AccountingReady {
		snapshot := stats
		s.lastStats.Store(&snapshot)
	} else if prior := s.lastStats.Load(); prior != nil {
		stats = *prior
		stats.AccountingReady = s.closed.Load()
	}
	stats.Accepted, stats.Delivered, stats.Retried = s.accepted.Load(), s.delivered.Load(), s.retried.Load()
	stats.PrunedAge, stats.PrunedBytes, stats.Malformed = s.prunedAge.Load(), s.prunedBytes.Load(), s.malformed.Load()
	stats.AdmissionFailures = s.admissionFailures.Load()
	if source := s.lossSource.Load(); source != nil {
		loss := source.sample()
		stats.QueueDropped = loss.Dropped
		stats.ExportFailures = loss.ExportFailures
	}
	return stats
}

func (s *azureReplaySpool) close(ctx context.Context) error {
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	defer func() {
		activeAzureReplaySpools.Lock()
		delete(activeAzureReplaySpools.spools, s)
		activeAzureReplaySpools.Unlock()
	}()
	s.cancel()
	select {
	case <-s.healthDone:
	case <-ctx.Done():
	}
	select {
	case <-s.done:
	case <-ctx.Done():
		go func() { <-s.done; _ = s.stats(); _ = s.index.release(context.Background()) }()
		return ctx.Err()
	}
	err := s.drain(ctx)
	_ = s.stats()
	return errors.Join(err, s.index.release(ctx))
}
