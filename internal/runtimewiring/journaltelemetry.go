package runtimewiring

import (
	"context"
	"errors"
	"fmt"
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
	Roots map[string]*commandJournalTelemetryOwner
}{Roots: make(map[string]*commandJournalTelemetryOwner)}

type commandJournalTelemetryOwner struct {
	Client *telemetry.Client
	Users  int
}

// StartCommandJournalTelemetry owns only this known instance root, never an
// ambient process-wide sink. Callers close their journal writers before stop.
func StartCommandJournalTelemetry(l instance.Layout, stderr io.Writer) func() {
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
	if owner := commandJournalTelemetry.Roots[root]; owner != nil {
		owner.Users++
		return releaseCommandJournalTelemetry(root, owner, stderr)
	}
	if journal.HasCommittedEventSink(l.Root) {
		return noop
	}
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(stderr, "warning: journal OTLP logs unavailable: %v\n", err)
		}
		return noop
	}
	azureEnabled := cfg.Telemetry.AzureMonitor != nil && cfg.Telemetry.AzureMonitor.Enabled() && cfg.Telemetry.EffectiveCollectionProfile().IncludesJournal()
	otlpEnabled := cfg.Telemetry.OTLP != nil && cfg.Telemetry.OTLP.JournalLogsEnabled()
	if !cfg.TelemetryEnabled() || (!otlpEnabled && !azureEnabled && !cfg.Telemetry.NamedJournalEnabled()) {
		return noop
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: journal OTLP logs unavailable: %v\n", err)
		return noop
	}
	registry, scrubber := journal.DefaultScrubber()
	initialize, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	export := telemetry.Config{
		ServiceName: "goobers", ServiceVersion: version.Get().Version, BuildCommit: version.Get().Commit,
		Scrubber: scrubber, JournalRoot: l.Root, JournalLogsOnly: true,
	}
	export.JournalInstanceID, _ = l.ReadIdentity()
	if cfg.Telemetry.OTLP != nil {
		if err := ConfigureOTLP(initialize, &export, *cfg.Telemetry.OTLP, registry, stores); err != nil {
			cancel()
			_, _ = fmt.Fprintf(stderr, "warning: journal telemetry unavailable: %v\n", err)
			return noop
		}
	}
	if cfg.Telemetry.AzureMonitor != nil {
		if err := ConfigureAzureMonitor(initialize, &export, *cfg.Telemetry.AzureMonitor, cfg.Telemetry.EffectiveCollectionProfile(), l.Root, registry, stores); err != nil {
			cancel()
			_, _ = fmt.Fprintf(stderr, "warning: journal telemetry unavailable: %v\n", err)
			return noop
		}
	}
	namedErr := ConfigureNamedTelemetry(initialize, &export, cfg.Telemetry, l.Root, registry, stores, false)
	client, err := telemetry.New(initialize, export)
	err = errors.Join(namedErr, err)
	cancel()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: journal telemetry unavailable: %v\n", err)
	}
	if client == nil {
		return noop
	}
	owner := &commandJournalTelemetryOwner{Client: client, Users: 1}
	commandJournalTelemetry.Roots[root] = owner
	return releaseCommandJournalTelemetry(root, owner, stderr)
}

func releaseCommandJournalTelemetry(root string, owner *commandJournalTelemetryOwner, stderr io.Writer) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			commandJournalTelemetry.Lock()
			defer commandJournalTelemetry.Unlock()
			owner.Users--
			if owner.Users > 0 {
				return
			}
			defer delete(commandJournalTelemetry.Roots, root)
			// A canceled command must still get a bounded chance to drain events.
			flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := owner.Client.Shutdown(flush); err != nil {
				_, _ = fmt.Fprintf(stderr, "warning: journal telemetry shutdown: %v\n", err)
			}
			stats := owner.Client.JournalExportStats()
			if stats.Dropped > 0 || stats.ExportFailures > 0 {
				_, _ = fmt.Fprintf(stderr, "warning: journal telemetry: %d dropped, %d export failures; local journal remains authoritative\n", stats.Dropped, stats.ExportFailures)
			}
			if stats.SinkPanics > 0 {
				_, _ = fmt.Fprintf(stderr, "warning: journal sinks: %d contained panics across this process; local journals remain authoritative\n", stats.SinkPanics)
			}
		})
	}
}
