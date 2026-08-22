package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/goobers/goobers/internal/configtree"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

var configReloadInterval = time.Second

type openPRLoop struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func newOpenPRLoop(ctx context.Context, refresher *localscheduler.OpenPRRefresherSet) *openPRLoop {
	loop := &openPRLoop{ctx: ctx}
	loop.Replace(refresher)
	return loop
}

func (l *openPRLoop) Replace(refresher *localscheduler.OpenPRRefresherSet) {
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
	layout    instance.Layout
	setup     *schedulerSetup
	scheduler *localscheduler.Scheduler
	openPRs   *openPRLoop
	reads     *readservice.Local
	// mu serializes poll()/pollOnce() across the reloader's two independent
	// callers (Run's own ticker, gated behind --watch-config, and #459's
	// on-demand apply sweep, which is unconditional). Before #459, poll()
	// only ever ran on Run's single goroutine; now a concurrent `goobers
	// apply` while --watch-config is also on would otherwise race on every
	// field below plus the non-idempotent scheduler.Reload/RunnerRegistry
	// .Replace/openPRs.Replace side effects poll performs.
	mu sync.Mutex
	// readModel publishes a definitions-changed row into the change feed
	// (#1929). Replaces the deleted poller's out-of-band publish, so a config
	// reload reaches clients through the SAME ordered feed as everything else
	// rather than a second channel with its own latency.
	readModel       *readmodel.Store
	wg              *sync.WaitGroup
	appliedDigest   string
	observedDigest  string
	lastDigestError string
	// lastRejectionMessage is set by reject() during the most recent poll
	// call and cleared at the start of each pollOnce (#459) — it lets an
	// on-demand caller distinguish a validation rejection from "nothing to
	// do," which poll's own plain error return cannot (reject reports success
	// once the rejection is durably journaled).
	lastRejectionMessage string
}

func (r *configReloader) Run(ctx context.Context) error {
	ticker := time.NewTicker(configReloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			r.mu.Lock()
			err := r.poll(now)
			r.mu.Unlock()
			if err != nil {
				return err
			}
		}
	}
}

// pollOnce runs exactly one reload check — the same check the ticker in Run
// performs every tick — and reports a structured outcome instead of just
// success/failure, so an on-demand caller (goobers apply, #459) can
// distinguish "nothing changed," "applied," and "rejected: <message>."
func (r *configReloader) pollOnce(now time.Time) (applied bool, oldDigest, newDigest, rejected string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	oldDigest = r.appliedDigest
	r.lastRejectionMessage = ""
	if pollErr := r.poll(now); pollErr != nil {
		return false, oldDigest, oldDigest, "", pollErr
	}
	if r.appliedDigest != oldDigest {
		return true, oldDigest, r.appliedDigest, "", nil
	}
	return false, oldDigest, oldDigest, r.lastRejectionMessage, nil
}

// poll runs one reload check. Callers must hold r.mu — poll freely mutates
// appliedDigest/observedDigest/lastDigestError/lastRejectionMessage and
// drives non-idempotent side effects (scheduler.Reload, RunnerRegistry
// .Replace, openPRs.Replace) with no internal synchronization of its own.
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
	if webhookListenerTopologyChanged(r.setup.Definitions, set, r.setup.Config) {
		return r.reject(digest, errors.New("adding the first or removing the last webhook trigger requires a daemon restart"))
	}
	runtimeMigration, err := r.layout.MigrateLegacyRuntimeWithReport(configuredGaggleNames(set))
	if err != nil {
		return r.reject(digest, err)
	}
	if err := journalLegacyRuntimeMigration(r.layout, r.setup.InstanceLog, runtimeMigration); err != nil {
		return r.reject(digest, fmt.Errorf("journal legacy runtime migration: %w", err))
	}
	definitions, err := buildSchedulerDefinitions(
		r.layout,
		r.setup.Config,
		set,
		report,
		r.wg,
		r.setup.RunnerRegistry,
		r.setup.Telemetry,
		r.setup.RollupDB,
		r.setup.Watermarks,
		r.setup.InstanceLog,
		r.setup.SharedRegistry,
		r.setup.WorktreesByGaggle,
		r.setup.ProviderQuota,
		r.setup.TerminalNotifier,
		r.setup.SecretStores,
		nil,
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
		r.observedDigest = r.appliedDigest
		return err
	}
	r.setup.RunnerRegistry.Replace(definitions.Runners)
	r.setup.Interventions.Replace(interventionDefinitions(definitions, r.setup.LegacyRunner))
	r.setup.Runner = definitions.Runner
	r.setup.Runners = definitions.Runners
	r.setup.Definitions = definitions.Set
	r.setup.Validation = definitions.Validation
	r.setup.Entries = definitions.Entries
	r.setup.Machines = definitions.Machines
	r.setup.GooberDigests = definitions.GooberDigests
	r.setup.RepoRefs = definitions.RepoRefs
	r.setup.OpenPRRefresher = definitions.OpenPRRefresher
	r.setup.Worktrees = definitions.Worktrees
	r.setup.WorktreesByGaggle = definitions.WorktreesByGaggle
	r.openPRs.Replace(definitions.OpenPRRefresher)
	if err := r.reads.ReloadDefinitions(definitions.Set, definitions.Validation, now); err != nil {
		r.observedDigest = r.appliedDigest
		return fmt.Errorf("reload read service definitions: %w", err)
	}
	if r.readModel != nil {
		// Logged, not fatal: a reload that applied correctly must not be
		// reported as failed because its invalidation could not be recorded.
		// The cost is that connected clients notice the new definitions on
		// their next ordinary refresh instead of immediately.
		if err := r.readModel.PublishDefinitionsChanged(context.Background()); err != nil {
			log.Printf("config reload: publish definitions change: %v", err)
		}
	}
	// #3376: the applied edit just superseded the workflow digest every
	// in-flight run is pinned to. Report which of those runs a restart can
	// still resume from their pinned snapshot and which one would refuse —
	// logged, never fatal, since an applied reload must not be reported as
	// failed because its advisory report could not be written.
	if drift, driftErr := inspectWorkflowDigestDrift(r.layout, r.setup.Machines); driftErr != nil {
		log.Printf("config reload: inspect workflow digest drift: %v", driftErr)
	} else if driftErr := journalWorkflowDigestDrift(r.setup.InstanceLog, drift); driftErr != nil {
		log.Printf("config reload: journal workflow digest drift: %v", driftErr)
	}
	r.appliedDigest = digest
	return nil
}

func (r *configReloader) reject(newDigest string, reloadErr error) error {
	message := configReloadErrorMessage(reloadErr)
	r.lastRejectionMessage = message
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
// tell a real change from a no-op. It tracks YAML definitions, their referenced
// goober instructions and skill bodies, and every file in a goober assets
// directory; unrelated config-tree churn remains excluded.
func configDirectoryDigest(root string) (string, error) {
	hash := sha256.New()
	contentPaths := make(map[string]struct{})
	writeEntry := func(path string, mode fs.FileMode, content []byte) error {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(relative)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(relative))
		binary.BigEndian.PutUint64(size[:], uint64(mode))
		_, _ = hash.Write(size[:])
		binary.BigEndian.PutUint64(size[:], uint64(len(content)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write(content)
		return nil
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() && configtree.IsGaggleSkillsDir(root, path) {
			return filepath.SkipDir
		}
		if gooberassets.IsSourceDir(path) {
			bundle, err := gooberassets.Load(path)
			if err != nil {
				return err
			}
			if bundle == nil {
				return nil
			}
			if err := writeEntry(path, 0, []byte(bundle.Fingerprint())); err != nil {
				return err
			}
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			// Skip dotfile directories (notably .git when the config dir is a
			// tracked repo, per the Workflow-CD epic #453) and all their churn.
			if path != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		// Outside asset bundles, only YAML definitions contribute.
		if strings.HasPrefix(name, ".") {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
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
		if err := writeEntry(path, 0, content); err != nil {
			return err
		}
		references, err := gooberContentReferences(root, path, content)
		if err != nil {
			return err
		}
		for _, contentPath := range references {
			contentPaths[contentPath] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("digest config directory: %w", err)
	}
	paths := make([]string, 0, len(contentPaths))
	for path := range contentPaths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("digest config directory: stat resolved goober content: %w", err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("digest config directory: read resolved goober content: %w", err)
		}
		if err := writeEntry(path, 0, content); err != nil {
			return "", fmt.Errorf("digest config directory: %w", err)
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

type configDigestDocument struct {
	Kind string `json:"kind"`
	Spec struct {
		Instructions string   `json:"instructions"`
		Gaggle       string   `json:"gaggle"`
		Skills       []string `json:"skills"`
	} `json:"spec"`
}

func gooberContentReferences(configDir, definitionPath string, content []byte) ([]string, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(content), 4096)
	var paths []string
	for {
		var document configDigestDocument
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return paths, nil
		}
		if err != nil {
			// The YAML bytes already move the digest; config validation owns the
			// diagnostic for malformed documents.
			return paths, nil
		}
		if document.Kind != "Goober" {
			continue
		}
		if document.Spec.Instructions != "" {
			paths = append(paths, filepath.Join(filepath.Dir(definitionPath), document.Spec.Instructions))
		}
		for _, skill := range document.Spec.Skills {
			_, skillPaths, ok, err := skillPackagePaths(configDir, document.Spec.Gaggle, skill)
			if err != nil {
				return nil, fmt.Errorf("list referenced skill %q package: %w", skill, err)
			}
			if ok {
				paths = append(paths, skillPaths...)
			}
		}
	}
}
