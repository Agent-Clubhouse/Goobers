package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/version"
)

var commandJournalTelemetry = struct {
	sync.Mutex
	roots map[string]*commandJournalTelemetryOwner
}{roots: make(map[string]*commandJournalTelemetryOwner)}

type commandJournalTelemetryOwner struct {
	client *telemetry.Client
	users  int
}

// startCommandJournalTelemetry owns only this known instance root, never an
// ambient process-wide sink. Callers close their journal writers before stop.
func startCommandJournalTelemetry(l instance.Layout, stderr io.Writer) func() {
	noop := func() {}
	root, err := filepath.Abs(l.Root)
	if err != nil {
		return noop
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return noop
	}
	if runtime.GOOS == "windows" {
		root = strings.ToLower(root)
	}
	commandJournalTelemetry.Lock()
	defer commandJournalTelemetry.Unlock()
	if owner := commandJournalTelemetry.roots[root]; owner != nil {
		owner.users++
		return releaseCommandJournalTelemetry(root, owner, stderr)
	}
	if journal.HasCommittedEventSink(l.Root) {
		return noop
	}
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			pf(stderr, "warning: journal OTLP logs unavailable: %v\n", err)
		}
		return noop
	}
	if !cfg.TelemetryEnabled() || cfg.Telemetry.OTLP == nil || !cfg.Telemetry.OTLP.JournalLogsEnabled() {
		return noop
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		pf(stderr, "warning: journal OTLP logs unavailable: %v\n", err)
		return noop
	}
	registry, scrubber := journal.DefaultScrubber()
	initialize, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	export := telemetry.Config{
		ServiceName: "goobers", ServiceVersion: version.Get().Version, BuildCommit: version.Get().Commit,
		Scrubber: scrubber, JournalRoot: l.Root, JournalLogsOnly: true,
	}
	export.JournalInstanceID, _ = l.ReadIdentity()
	if err := configureOTLP(initialize, &export, *cfg.Telemetry.OTLP, registry, stores); err != nil {
		cancel()
		pf(stderr, "warning: journal OTLP logs unavailable: %v\n", err)
		return noop
	}
	client, err := telemetry.New(initialize, export)
	cancel()
	if err != nil {
		pf(stderr, "warning: journal OTLP logs unavailable: %v\n", err)
	}
	if client == nil {
		return noop
	}
	owner := &commandJournalTelemetryOwner{client: client, users: 1}
	commandJournalTelemetry.roots[root] = owner
	return releaseCommandJournalTelemetry(root, owner, stderr)
}

func releaseCommandJournalTelemetry(root string, owner *commandJournalTelemetryOwner, stderr io.Writer) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			commandJournalTelemetry.Lock()
			defer commandJournalTelemetry.Unlock()
			owner.users--
			if owner.users > 0 {
				return
			}
			defer delete(commandJournalTelemetry.roots, root)
			// A canceled command must still get a bounded chance to drain events.
			flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := owner.client.Shutdown(flush); err != nil {
				pf(stderr, "warning: journal OTLP logs shutdown: %v\n", err)
			}
			stats := owner.client.JournalExportStats()
			if stats.Dropped > 0 || stats.ExportFailures > 0 {
				pf(stderr, "warning: journal OTLP logs: %d dropped, %d export failures; local journal remains authoritative\n", stats.Dropped, stats.ExportFailures)
			}
			if stats.SinkPanics > 0 {
				pf(stderr, "warning: journal sinks: %d contained panics across this process; local journals remain authoritative\n", stats.SinkPanics)
			}
		})
	}
}
