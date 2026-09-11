package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/selfupdate"
	"github.com/goobers/goobers/internal/version"
)

// updateCheckResult carries one rendered operator notice from the background
// checker to the daemon loop. The checker never writes to stdout itself:
// stdout is not safe for concurrent use (see the shutdown join in up.go), so
// the daemon loop stays the single writer and the checker only hands it text.
type updateCheckResult struct {
	notice string
	// warning is a non-fatal diagnostic. A release check that fails tells the
	// operator nothing they need to act on, so it is reported once and never
	// affects the daemon's health or exit status.
	warning string
}

// report renders one result. The daemon loop owns stdout/stderr, so the
// checker hands it text and this method does the writing on the loop's
// goroutine.
func (r updateCheckResult) report(stdout, stderr io.Writer) {
	if r.notice != "" {
		pf(stdout, "%s\n", r.notice)
	}
	if r.warning != "" {
		pf(stderr, "warning: %s\n", r.warning)
	}
}

// updateChecker is the daemon's notify-only release check (#4903). It reports
// when the newest release on the configured channel is ahead of the running
// build and never applies anything: staging an update stays an explicit
// operator action (INST-019).
type updateChecker struct {
	root     string
	settings instance.UpdateCheckConfig
	interval time.Duration
	current  string
	check    func(context.Context, selfupdate.CheckOptions) (selfupdate.CheckResult, error)
	// announced is the version last reported to the operator. The checker
	// announces only when the resolved version CHANGES, so an operator who
	// does not act on v1.2.0 is not told about v1.2.0 again on every tick.
	announced string
	// warned records that a check failure has already been reported, so a
	// persistently unreachable API produces one line rather than one per tick.
	warned bool
}

// startUpdateCheck launches the background checker and returns the channel the
// daemon loop renders from plus a done channel to join at shutdown. A disabled
// check returns a nil results channel — receiving from nil blocks forever,
// which is exactly the "never fires" the caller wants — and an already-closed
// done, so the caller's shutdown join needs no special case.
func startUpdateCheck(
	ctx context.Context,
	root string,
	cfg *instance.Config,
	stderr io.Writer,
) (<-chan updateCheckResult, <-chan struct{}) {
	disabled := make(chan struct{})
	close(disabled)
	settings := cfg.UpdateCheckSettings()
	if !settings.EnabledEffective() {
		return nil, disabled
	}
	interval, err := settings.IntervalDuration()
	if err != nil {
		// Validation already rejects this at load; a surviving error means the
		// daemon should still run, just without the check.
		pf(stderr, "warning: update check disabled: %v\n", err)
		return nil, disabled
	}
	checker := &updateChecker{
		root:     root,
		settings: settings,
		interval: interval,
		current:  version.Get().Version,
		check:    selfupdate.CheckLatest,
	}
	results := make(chan updateCheckResult, 1)
	done := make(chan struct{})
	go checker.run(ctx, results, done)
	return results, done
}

// run checks once immediately — the startup notice — then on the interval.
func (c *updateChecker) run(ctx context.Context, results chan<- updateCheckResult, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		if result, ok := c.once(ctx); ok {
			select {
			case results <- result:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// once performs one check and reports whether it produced something to say.
func (c *updateChecker) once(ctx context.Context) (updateCheckResult, bool) {
	checkCtx, cancel := context.WithTimeout(ctx, selfupdate.DefaultCheckTimeout)
	defer cancel()
	result, err := c.check(checkCtx, selfupdate.CheckOptions{
		CurrentVersion: c.current,
		Owner:          c.settings.Owner,
		Repository:     c.settings.Repository,
		Channel:        c.settings.ChannelEffective(),
	})
	if err != nil {
		// A build with no comparable version (`dev`, i.e. every plain
		// `go build`) has nothing to compare against. That is the normal
		// state of a development daemon, not a problem to report.
		if errors.Is(err, selfupdate.ErrVersionNotComparable) || ctx.Err() != nil {
			return updateCheckResult{}, false
		}
		if c.warned {
			return updateCheckResult{}, false
		}
		c.warned = true
		return updateCheckResult{warning: fmt.Sprintf("update check unavailable: %v", err)}, true
	}
	c.warned = false
	// Cache for `goobers status` so a status call renders the same answer
	// without making a request of its own.
	if err := selfupdate.WriteCheck(c.root, result); err != nil {
		c.warned = true
		return updateCheckResult{warning: fmt.Sprintf("update check cache: %v", err)}, true
	}
	if !result.UpdateAvailable || result.LatestVersion == c.announced {
		return updateCheckResult{}, false
	}
	c.announced = result.LatestVersion
	return updateCheckResult{notice: selfupdate.Notice(result, selfupdate.Supervised(c.root))}, true
}

// reportFleetConnectorStopped reports a Fleet connector that stopped on its
// own. A connector that stops because the daemon is shutting down is expected
// and says nothing. Extracted from the daemon loop's select so the loop stays
// under the complexity gate's baseline.
func reportFleetConnectorStopped(stderr io.Writer, connectorErr, ctxErr error) {
	if ctxErr != nil {
		return
	}
	if connectorErr != nil {
		pf(stderr, "warning: Fleet connector stopped: %v\n", connectorErr)
		return
	}
	pln(stderr, "Fleet connector stopped")
}
