package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readservice"
)

var configReloadInterval = time.Second

type openPRLoop struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func newOpenPRLoop(ctx context.Context, refresher *localscheduler.OpenPRRefresher) *openPRLoop {
	loop := &openPRLoop{ctx: ctx}
	loop.Replace(refresher)
	return loop
}

func (l *openPRLoop) Replace(refresher *localscheduler.OpenPRRefresher) {
	l.stopCurrent()
	if refresher == nil || l.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithCancel(l.ctx)
	done := make(chan struct{})
	l.cancel = cancel
	l.done = done
	go func() {
		defer close(done)
		refresher.Run(ctx)
	}()
}

func (l *openPRLoop) Stop() {
	l.stopCurrent()
}

func (l *openPRLoop) stopCurrent() {
	if l.cancel == nil {
		return
	}
	l.cancel()
	<-l.done
	l.cancel = nil
	l.done = nil
}

type configReloader struct {
	layout          instance.Layout
	setup           *schedulerSetup
	scheduler       *localscheduler.Scheduler
	openPRs         *openPRLoop
	reads           *readservice.Local
	events          *httpapi.EventStream
	wg              *sync.WaitGroup
	appliedDigest   string
	observedDigest  string
	lastDigestError string
}

func (r *configReloader) Run(ctx context.Context) error {
	ticker := time.NewTicker(configReloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if err := r.poll(now); err != nil {
				return err
			}
		}
	}
}

func (r *configReloader) poll(now time.Time) error {
	digest, err := configDirectoryDigest(r.layout.ConfigDir())
	if err != nil {
		message := err.Error()
		if message == r.lastDigestError {
			return nil
		}
		r.lastDigestError = message
		return r.reject("", err)
	}
	r.lastDigestError = ""
	if digest == r.observedDigest {
		return nil
	}
	r.observedDigest = digest
	if digest == r.appliedDigest {
		return nil
	}

	set, report, err := loadConfigDirectory(r.layout.ConfigDir())
	if err != nil {
		return r.reject(digest, &configReportError{
			report: report,
			err:    fmt.Errorf("config directory invalid: %w", err),
		})
	}
	if webhookListenerTopologyChanged(r.setup.Definitions, set) {
		return r.reject(digest, errors.New("adding the first or removing the last webhook trigger requires a daemon restart"))
	}
	if err := r.layout.MigrateLegacyRuntime(configuredGaggleNames(set)); err != nil {
		return r.reject(digest, err)
	}
	definitions, err := buildSchedulerDefinitions(
		r.layout,
		r.setup.Config,
		set,
		report,
		r.wg,
		r.setup.Telemetry,
		r.setup.RollupDB,
		r.setup.InstanceLog,
		r.setup.SharedRegistry,
		r.setup.WorktreesByGaggle,
		r.setup.ProviderQuota,
		r.setup.TerminalNotifier,
	)
	if err != nil {
		return r.reject(digest, &configReportError{report: report, err: err})
	}

	stableDigest, err := configDirectoryDigest(r.layout.ConfigDir())
	if err != nil {
		r.observedDigest = r.appliedDigest
		return r.reject(digest, err)
	}
	if stableDigest != digest {
		r.observedDigest = r.appliedDigest
		return nil
	}
	if err := r.scheduler.Reload(definitions.Entries, definitions.OpenPRRefresher, now, r.appliedDigest, digest); err != nil {
		return err
	}
	r.setup.Runner = definitions.Runner
	r.setup.Runners = definitions.Runners
	r.setup.Worktrees = definitions.Worktrees
	r.setup.WorktreesByGaggle = definitions.WorktreesByGaggle
	r.openPRs.Replace(definitions.OpenPRRefresher)
	if err := r.reads.ReloadDefinitions(definitions.Set, definitions.Validation, now); err != nil {
		return fmt.Errorf("reload read service definitions: %w", err)
	}
	if r.events != nil {
		r.events.PublishDefinitionsChanged()
	}
	r.appliedDigest = digest
	return nil
}

func (r *configReloader) reject(newDigest string, reloadErr error) error {
	message := configReloadErrorMessage(reloadErr)
	event := journal.Event{
		Type: journal.EventConfigReloadRejected,
		Error: &journal.ErrorDetail{
			Code:    "config_reload_rejected",
			Message: message,
		},
		Runner: map[string]any{"oldDigest": r.appliedDigest},
	}
	if newDigest != "" {
		event.Runner["newDigest"] = newDigest
	}
	// The instance journal is the durable provenance contract. If it cannot
	// record the rejection, propagate the error so the daemon fails closed.
	if err := r.setup.InstanceLog.Append(event); err != nil {
		return fmt.Errorf("journal rejected config reload: %w", err)
	}
	return nil
}

func configReloadErrorMessage(err error) string {
	var reportErr *configReportError
	if !errors.As(err, &reportErr) || reportErr.report == nil {
		return err.Error()
	}
	parts := make([]string, 0, len(reportErr.report.Issues)+1)
	parts = append(parts, err.Error())
	for _, issue := range reportErr.report.Issues {
		parts = append(parts, issue.String())
	}
	return strings.Join(parts, "; ")
}

// configDirectoryDigest fingerprints the config directory so the reloader can
// tell a real change from a no-op. Its file-selection surface MUST match what
// instance.LoadConfigDir / validate.ValidateDir actually consume (readDocs in
// internal/instance/configdir.go: .yaml/.yml only). Hashing a wider surface is
// a footgun: churn in non-config files (a README, a tracked repo's .git/) would
// journal spurious config.reloaded events, and — worse — an editor's atomic
// write-then-rename or a git checkout racing WalkDir would surface a transient
// read error that the poll loop records as a false config.reload.rejected.
func configDirectoryDigest(root string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() {
			// Skip dotfile directories (notably .git when the config dir is a
			// tracked repo, per the Workflow-CD epic #453) and all their churn.
			if path != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		// Only the file types the loader consumes contribute to the digest.
		// This filters out dotfiles, editor swap/backup/temp files (.swp, ~,
		// vim's 4913, #file#), and any other non-config content.
		if strings.HasPrefix(name, ".") {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			// A config file that vanished between the walk and the read (an
			// editor's atomic rename, a git checkout) is a transient state, not
			// a rejectable config. Skip it and let the next poll — with the
			// read-validate-reread stability check — converge on settled bytes.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		relative = filepath.ToSlash(relative)
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(relative)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(relative))
		binary.BigEndian.PutUint64(size[:], uint64(len(content)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write(content)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("digest config directory: %w", err)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
