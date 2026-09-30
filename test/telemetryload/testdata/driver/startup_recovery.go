package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

var startupRecoveryActive atomic.Bool
var startupRecoveryDuplicates atomic.Int64
var startupRecordID = regexp.MustCompile(`^(?:[a-f0-9]{32}|[a-f0-9]{64})$`)

// The timed lifecycle is intentionally unchanged. This receipt measures a
// separate restart of that stopped fixture, after synthetic seed files have
// been removed, so deferred admission is not mistaken for lost telemetry.
type startupRecoveryReceipt struct {
	StartedUTC, FinishedUTC time.Time
	Expected, Seen          int
	Missing                 int
	Requests                int64
	Duplicates              int64
	StartupMS, ShutdownMS   float64
	AccountingReady         bool
	PendingRecords          int
	NoPending               bool
	Health                  startupHealthAudit
	Error                   string
}

func (r *startupRecoveryReceipt) Successful() bool {
	if r == nil || r.Error != "" {
		return false
	}
	if r.NoPending {
		return r.Expected == 0 && r.Requests > 0
	}
	return r.Expected > 0 && r.Seen == r.Expected && r.Missing == 0 && r.Requests > 0 &&
		r.AccountingReady && r.PendingRecords == 0 && r.ShutdownMS > 0 && r.ShutdownMS <= 15250 &&
		r.Health.complete(true, collectionProfile) && r.Health.clean("healthy")
}

func startupPendingRecordIDs(root string) ([]string, error) {
	var ids []string
	seen := make(map[string]bool)
	for _, stream := range []string{"traces", "journal", "diagnostics"} {
		for _, dir := range []string{filepath.Join(spool(root), stream), filepath.Join(spool(root), ".replay-bootstrap", stream)} {
			if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return nil, err
			}
			err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("unexpected startup replay symlink: %s", path)
				}
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ndjson") {
					return nil
				}
				if !entry.Type().IsRegular() {
					return fmt.Errorf("startup replay is not a regular file: %s", path)
				}
				file, err := os.Open(path)
				if err != nil {
					return err
				}
				defer file.Close()
				scan := bufio.NewScanner(file)
				scan.Buffer(make([]byte, 64*1024), 8<<20)
				if !scan.Scan() {
					return fmt.Errorf("startup replay file has no header: %s", path)
				}
				var header struct{ Schema string }
				if err := json.Unmarshal(scan.Bytes(), &header); err != nil || header.Schema != "goobers.dev/telemetry/azure-replay/v1" {
					return fmt.Errorf("invalid startup replay header in %s: %v", path, err)
				}
				for scan.Scan() {
					var record struct {
						Data struct {
							BaseData struct{ Properties map[string]string }
						}
					}
					if err := json.Unmarshal(scan.Bytes(), &record); err != nil {
						return fmt.Errorf("decode startup replay record: %w", err)
					}
					id := record.Data.BaseData.Properties["goobers.telemetry.record_id"]
					if !startupRecordID.MatchString(id) || seen[id] {
						return fmt.Errorf("malformed or duplicate startup replay identity in %s", path)
					}
					seen[id] = true
					ids = append(ids, id)
				}
				return scan.Err()
			})
			if err != nil {
				return nil, err
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func consumeStartupRecovery(w http.ResponseWriter, decoded io.Reader, responseMode int32) {
	scan := bufio.NewScanner(decoded)
	scan.Buffer(make([]byte, 64*1024), 8<<20)
	var batch []string
	for scan.Scan() {
		var record struct {
			Data struct {
				BaseData struct{ Properties map[string]string }
			}
		}
		if err := json.Unmarshal(scan.Bytes(), &record); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id := record.Data.BaseData.Properties["goobers.telemetry.record_id"]
		if !startupRecordID.MatchString(id) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		batch = append(batch, id)
	}
	if scan.Err() != nil || len(batch) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if responseMode != 0 {
		rejects.Add(1)
		respondNetworkFault(w, responseMode)
		return
	}
	for _, id := range batch {
		if _, old := ids.LoadOrStore(id, true); old {
			startupRecoveryDuplicates.Add(1)
		}
	}
	w.WriteHeader(http.StatusOK)
}

func recoverStartupRecords(name, root, api string, expected []string) *startupRecoveryReceipt {
	r := &startupRecoveryReceipt{StartedUTC: time.Now().UTC(), Expected: len(expected), Missing: len(expected)}
	write(filepath.Join(out, name+"-expected-record-ids.txt"), strings.Join(expected, "\n")+"\n")
	for _, id := range expected {
		ids.Delete(id)
	}
	startupRecoveryDuplicates.Store(0)
	startupRecoveryActive.Store(true)
	defer startupRecoveryActive.Store(false)
	if err := runStartupRecovery(r, name, root, api, expected); err != nil {
		r.Error = err.Error()
	}
	r.FinishedUTC = time.Now().UTC()
	return r
}

func runStartupRecovery(r *startupRecoveryReceipt, name, root, api string, expected []string) error {
	log, err := os.Create(filepath.Join(out, name+"-recovery-daemon.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	process := cmd(context.Background(), "up", "--drain-timeout", "15s", root)
	process.Stdout, process.Stderr = log, log
	requestBase := requests.Load()
	defer func() {
		r.Requests = requests.Load() - requestBase
		r.Duplicates = startupRecoveryDuplicates.Load()
	}()
	started := time.Now()
	if err := process.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	exited := false
	defer func() {
		if !exited {
			_ = process.Process.Kill()
			<-done
		}
	}()
	readyBy := time.Now().Add(time.Minute)
	for !startupReady(api) {
		select {
		case err := <-done:
			exited = true
			return fmt.Errorf("recovery daemon exited before readiness: %v", err)
		default:
		}
		if time.Now().After(readyBy) {
			return errors.New("recovery daemon readiness deadline exceeded")
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.StartupMS = float64(time.Since(started)) / float64(time.Millisecond)
	deliveryBy := time.Now().Add(3 * time.Minute)
	for {
		seen := 0
		for _, id := range expected {
			if _, ok := ids.Load(id); ok {
				seen++
			}
		}
		r.Seen, r.Missing = seen, len(expected)-seen
		stats := telemetry.InspectAzureReplayRoot(spool(root))
		r.AccountingReady, r.PendingRecords = stats.AccountingReady, stats.PendingRecords
		if r.Missing == 0 && stats.AccountingReady && stats.PendingRecords == 0 {
			break
		}
		select {
		case err := <-done:
			exited = true
			return fmt.Errorf("recovery daemon exited before delivery: %v", err)
		default:
		}
		if time.Now().After(deliveryBy) {
			return fmt.Errorf("recovery deadline exceeded: missing=%d accountingReady=%v pending=%d", r.Missing, r.AccountingReady, r.PendingRecords)
		}
		time.Sleep(100 * time.Millisecond)
	}
	stopStarted := time.Now()
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	output, stopErr := cmd(stopCtx, "down", root).CombinedOutput()
	cancel()
	write(filepath.Join(out, name+"-recovery-down.log"), string(output))
	if stopErr != nil {
		return fmt.Errorf("recovery down: %w", stopErr)
	}
	select {
	case err := <-done:
		exited = true
		if err != nil {
			return fmt.Errorf("recovery daemon shutdown: %w", err)
		}
	case <-time.After(20 * time.Second):
		return errors.New("recovery daemon shutdown watchdog exceeded")
	}
	r.ShutdownMS = float64(time.Since(stopStarted)) / float64(time.Millisecond)
	if err := log.Close(); err != nil {
		return err
	}
	r.Health, err = auditStartupHealth(filepath.Join(out, name+"-recovery-daemon.log"))
	if err != nil {
		return err
	}
	stats := telemetry.InspectAzureReplayRoot(spool(root))
	r.AccountingReady, r.PendingRecords = stats.AccountingReady, stats.PendingRecords
	if !r.Health.complete(true, collectionProfile) || !r.Health.clean("healthy") {
		return errors.New("recovery shutdown health was incomplete or lossy")
	}
	return nil
}
