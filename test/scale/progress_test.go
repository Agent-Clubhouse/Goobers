package main

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// startScaleProgress complements the workflow's RSS/disk sampler with Go heap
// state and precise phases. It runs only in the opt-in large-scale test. Joining
// the sampler in cleanup prevents logs after testing has finished; no GC is
// forced and phase samples stay outside each operation's timed interval.
func startScaleProgress(t *testing.T) func(string) {
	t.Helper()
	started := time.Now()
	var mu sync.Mutex
	phase := "starting"
	sample := func(next string) {
		mu.Lock()
		defer mu.Unlock()
		if next != "" {
			phase = next
		}
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		t.Logf("scale progress: at=%s elapsed=%s phase=%s heap_alloc=%d heap_sys=%d heap_inuse=%d sys=%d total_alloc=%d gc=%d goroutines=%d",
			time.Now().UTC().Format(time.RFC3339), time.Since(started).Round(time.Millisecond), phase,
			stats.HeapAlloc, stats.HeapSys, stats.HeapInuse, stats.Sys, stats.TotalAlloc, stats.NumGC, runtime.NumGoroutine())
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sample("")
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
		sample("test:cleanup")
	})
	return sample
}
