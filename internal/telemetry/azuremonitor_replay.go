package telemetry

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
)

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
}

// AzureReplayStats is a low-cardinality snapshot of one signal spool.
type AzureReplayStats struct {
	Accepted, Delivered, Retried uint64
	PrunedAge, PrunedBytes       uint64
	Malformed                    uint64
	PendingRecords               int
	PendingBytes                 int64
	OldestPendingAge             time.Duration
}

var activeAzureReplaySpools = struct {
	sync.Mutex
	spools map[*azureReplaySpool]struct{}
}{spools: make(map[*azureReplaySpool]struct{})}

// Bounds span all three signal directories. This process-wide lock prevents
// concurrent workers from each observing room and collectively exceeding the
// configured per-instance byte ceiling.
var azureReplayBoundsMu sync.Mutex

// InspectAzureReplayRoot returns aggregate pending state across the trace,
// journal, and diagnostic subspools without starting an exporter.
func InspectAzureReplayRoot(root string) AzureReplayStats {
	var result AzureReplayStats
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
			createdAt, payload, err := readAzureReplayFile(filepath.Join(dir, entry.Name()))
			if err == nil {
				result.PendingRecords += azureReplayRecordCount(payload)
				age := time.Since(createdAt)
				if age > result.OldestPendingAge {
					result.OldestPendingAge = age
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
	}
	activeAzureReplaySpools.Unlock()
	return result
}

type azureReplaySpool struct {
	cfg    azureReplayConfig
	send   func(context.Context, []byte) error
	now    func() time.Time
	mu     sync.Mutex
	wake   chan struct{}
	done   chan struct{}
	cancel context.CancelFunc
	closed atomic.Bool

	accepted, delivered, retried atomic.Uint64
	prunedAge, prunedBytes       atomic.Uint64
	malformed                    atomic.Uint64
}

func newAzureReplaySpool(cfg azureReplayConfig, send func(context.Context, []byte) error) (*azureReplaySpool, error) {
	if cfg.dir == "" {
		return nil, nil
	}
	if cfg.maxAge <= 0 || cfg.maxBytes <= 0 {
		return nil, errors.New("azure monitor replay requires positive age and byte bounds")
	}
	if err := os.MkdirAll(cfg.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create Azure Monitor replay spool: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &azureReplaySpool{cfg: cfg, send: send, now: time.Now, wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel}
	activeAzureReplaySpools.Lock()
	activeAzureReplaySpools.spools[s] = struct{}{}
	activeAzureReplaySpools.Unlock()
	go s.run(ctx)
	s.signal()
	return s, nil
}

func (s *azureReplaySpool) submit(ctx context.Context, payload []byte) error {
	if s == nil {
		return errors.New("azure monitor replay spool is unavailable")
	}
	if s.closed.Load() {
		return errors.New("azure monitor replay spool is closed")
	}
	now := s.now().UTC()
	id, err := azureReplayID()
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%020d-%s%s", now.UnixNano(), id, azureReplayFileSuffix)
	azureReplayBoundsMu.Lock()
	s.mu.Lock()
	path, err := s.writeLocked(name, now, payload)
	if err == nil {
		s.accepted.Add(uint64(azureReplayRecordCount(payload)))
		err = s.enforceBoundsLocked(now, path)
	}
	s.mu.Unlock()
	azureReplayBoundsMu.Unlock()
	if err != nil {
		return err
	}
	s.signal()
	return s.drain(ctx)
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
	header, err := json.Marshal(azureReplayHeader{Schema: azureReplaySchema, CreatedAt: createdAt})
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

func (s *azureReplaySpool) enforceBoundsLocked(now time.Time, submitted string) error {
	files, err := s.boundFilesLocked()
	if err != nil {
		return err
	}
	var total int64
	for _, file := range files {
		createdAt, payload, readErr := readAzureReplayFile(file.path)
		if readErr != nil {
			s.malformed.Add(1)
			_ = os.Remove(file.path)
			continue
		}
		if now.Sub(createdAt) > s.cfg.maxAge {
			if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("prune expired Azure Monitor replay batch: %w", err)
			}
			s.prunedAge.Add(uint64(azureReplayRecordCount(payload)))
			continue
		}
		total += file.size
	}
	files, err = s.boundFilesLocked()
	if err != nil {
		return err
	}
	for _, file := range files {
		if total <= s.cfg.maxBytes {
			break
		}
		_, payload, readErr := readAzureReplayFile(file.path)
		if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("prune Azure Monitor replay batch: %w", err)
		}
		if readErr == nil {
			s.prunedBytes.Add(uint64(azureReplayRecordCount(payload)))
		}
		total -= file.size
	}
	if submitted != "" {
		if _, err := os.Stat(submitted); errors.Is(err, os.ErrNotExist) {
			return errors.New("azure monitor replay byte bound rejected the newest batch")
		}
	}
	return nil
}

type azureReplayFile struct {
	path string
	size int64
}

func (s *azureReplaySpool) filesLocked() ([]azureReplayFile, error) {
	return azureReplayFiles(s.cfg.dir)
}

func (s *azureReplaySpool) boundFilesLocked() ([]azureReplayFile, error) {
	if s.cfg.root == "" {
		return s.filesLocked()
	}
	var files []azureReplayFile
	for _, stream := range []string{"traces", "journal", "diagnostics"} {
		streamFiles, err := azureReplayFiles(filepath.Join(s.cfg.root, stream))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		files = append(files, streamFiles...)
	}
	sort.Slice(files, func(i, j int) bool {
		left, right := filepath.Base(files[i].path), filepath.Base(files[j].path)
		if left == right {
			return files[i].path < files[j].path
		}
		return left < right
	})
	return files, nil
}

func azureReplayFiles(dir string) ([]azureReplayFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list Azure Monitor replay spool: %w", err)
	}
	files := make([]azureReplayFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), azureReplayFileSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, azureReplayFile{path: filepath.Join(dir, entry.Name()), size: info.Size()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

func (s *azureReplaySpool) drain(ctx context.Context) error {
	azureReplayBoundsMu.Lock()
	defer azureReplayBoundsMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enforceBoundsLocked(s.now().UTC(), ""); err != nil {
		return err
	}
	files, err := s.filesLocked()
	if err != nil {
		return err
	}
	for _, file := range files {
		_, payload, err := readAzureReplayFile(file.path)
		if err != nil {
			s.malformed.Add(1)
			_ = os.Remove(file.path)
			continue
		}
		if err := s.send(ctx, payload); err != nil {
			s.retried.Add(1)
			return err
		}
		if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("acknowledge Azure Monitor replay batch: %w", err)
		}
		s.delivered.Add(uint64(azureReplayRecordCount(payload)))
	}
	return nil
}

func readAzureReplayFile(path string) (time.Time, []byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, nil, err
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReaderSize(file, azureReplayHeaderLimit)
	headerLine, err := reader.ReadString('\n')
	if err != nil || len(headerLine) > azureReplayHeaderLimit {
		return time.Time{}, nil, errors.New("invalid Azure Monitor replay header")
	}
	var header azureReplayHeader
	if err := json.Unmarshal([]byte(strings.TrimSuffix(headerLine, "\n")), &header); err != nil || header.Schema != azureReplaySchema || header.CreatedAt.IsZero() {
		return time.Time{}, nil, errors.New("invalid Azure Monitor replay header")
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
	return header.CreatedAt, payload, nil
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
		var timer *time.Timer
		if retrying {
			timer = time.NewTimer(jitterAzureReplayDelay(delay))
			retry = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-s.wake:
		case <-retry:
		}
		if timer != nil {
			timer.Stop()
		}
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.drain(attempt)
		cancel()
		if err == nil {
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
	stats := AzureReplayStats{
		Accepted: s.accepted.Load(), Delivered: s.delivered.Load(), Retried: s.retried.Load(),
		PrunedAge: s.prunedAge.Load(), PrunedBytes: s.prunedBytes.Load(), Malformed: s.malformed.Load(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := s.filesLocked()
	if err != nil {
		return stats
	}
	now := s.now()
	for _, file := range files {
		stats.PendingBytes += file.size
		createdAt, payload, err := readAzureReplayFile(file.path)
		if err == nil {
			stats.PendingRecords += azureReplayRecordCount(payload)
			if now.Sub(createdAt) > stats.OldestPendingAge {
				stats.OldestPendingAge = now.Sub(createdAt)
			}
		}
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
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.drain(ctx)
}
