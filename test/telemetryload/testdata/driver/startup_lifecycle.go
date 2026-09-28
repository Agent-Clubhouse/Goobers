package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
)

type startupTiming struct {
	StartedUTC, FinishedUTC time.Time
	StartupMS, ShutdownMS   float64
	Requests, Rejected      int64
}

func measureStartup(name, root, api string, waitForIndex bool) startupTiming {
	log, err := os.Create(filepath.Join(out, name+"-daemon.log"))
	must(err)
	defer func() { must(log.Close()) }()
	c := cmd(context.Background(), "up", "--drain-timeout", "15s", root)
	c.Stdout, c.Stderr = log, log
	c.Env = append(c.Env, "GODEBUG=gctrace=1")
	rq, rejected := requests.Load(), rejects.Load()
	started := time.Now()
	result := startupTiming{StartedUTC: started.UTC()}
	must(c.Start())
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	exited := false
	defer func() {
		if !exited {
			_ = c.Process.Kill()
			<-done
		}
	}()
	for !startupReady(api) {
		select {
		case err := <-done:
			exited = true
			panic(fmt.Sprintf("startup child exited before readiness: %v", err))
		default:
		}
		if time.Since(started) >= time.Minute {
			panic("startup readiness deadline exceeded")
		}
		time.Sleep(50 * time.Millisecond)
	}
	result.StartupMS = float64(time.Since(started).Nanoseconds()) / 1e6
	if waitForIndex {
		waitStartupIndex(root)
	}
	// No workflow execution: this isolates idle lifecycle with queued replay.
	// Both baseline and enabled retain the same post-readiness dwell.
	time.Sleep(time.Second)
	stopStarted := time.Now()
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	output, stopErr := cmd(stopCtx, "down", root).CombinedOutput()
	cancel()
	write(filepath.Join(out, name+"-down.log"), string(output))
	must(stopErr)
	select {
	case err := <-done:
		exited = true
		must(err)
	case <-time.After(20 * time.Second):
		panic("startup probe shutdown watchdog exceeded")
	}
	result.ShutdownMS = float64(time.Since(stopStarted).Nanoseconds()) / 1e6
	result.FinishedUTC = time.Now().UTC()
	result.Requests, result.Rejected = requests.Load()-rq, rejects.Load()-rejected
	return result
}

func startupReady(api string) bool {
	for _, path := range []string{"/readyz", "/api/v1/instance"} {
		response, err := client.Get(api + path)
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 {
			return false
		}
	}
	return true
}

func waitStartupIndex(root string) {
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		// Avoid the inspector's fallback directory scan before a manifest exists.
		if _, err := os.Stat(filepath.Join(spool(root), ".replay-index.db")); err == nil {
			if telemetry.InspectAzureReplayRoot(spool(root)).AccountingReady {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	panic("warm startup accounting deadline exceeded")
}

var startupSeedName = regexp.MustCompile(`^[0-9]{20}-seed\.ndjson$`)

// The only destructive action removes known synthetic seed files from a newly
// created, marked startup instance inside this invocation's output directory.
// Refuse symlinked path components and never recursively remove a directory.
func visitStartupSeeds(root string, remove bool) error {
	rel, err := filepath.Rel(out, root)
	if err != nil || filepath.Base(rel) != rel || !strings.HasPrefix(rel, "startup-") {
		return fmt.Errorf("startup fixture is outside the owned output directory")
	}
	marker, err := os.ReadFile(filepath.Join(root, ".startup-fixture"))
	if err != nil || string(marker) != "synthetic-startup-seeds-v1\n" {
		return fmt.Errorf("startup fixture ownership marker missing")
	}
	journal := filepath.Join(spool(root), "journal")
	for _, dir := range []string{root, filepath.Join(root, "telemetry-export"), spool(root), journal} {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("startup fixture path is not a real directory: %s", dir)
		}
	}
	entries, err := os.ReadDir(journal)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !startupSeedName.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("startup seed is not a regular file: %s", entry.Name())
		}
		if err := syncOrRemoveStartupSeed(filepath.Join(journal, entry.Name()), remove); err != nil {
			return err
		}
	}
	return nil
}

func syncOrRemoveStartupSeed(path string, remove bool) error {
	if remove {
		return os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
