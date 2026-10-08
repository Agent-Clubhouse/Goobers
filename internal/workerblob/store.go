// Package workerblob selects a resident worker's artifact transport and verifies
// directory sharing before a dispatch worker starts polling.
package workerblob

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/retryutil"
	"github.com/goobers/goobers/internal/startuphint"
)

// DefaultReadyWait bounds how long the startup probe waits for a daemon that
// is still starting and advertises nothing about its own startup. A rollout
// takes the daemon 4-9 minutes to reach ready (#6649); 20 minutes leaves
// generous headroom before a worker concludes the daemon is down and exits.
//
// A daemon that does advertise its startup budget and progress (#6895) moves
// the bound: see readinessBound.
const DefaultReadyWait = 20 * time.Minute

// DefaultMaxReadyWait caps the bound however far a daemon's advertised budget
// and progress extend it, so a daemon that keeps reporting progress without
// ever becoming ready cannot hold a worker forever.
const DefaultMaxReadyWait = 6 * time.Hour

// startupBudgetGrace is added to a daemon's advertised remaining budget: the
// budget is the daemon's own estimate, and missing it slightly is not "down".
const startupBudgetGrace = 5 * time.Minute

// ProbeOptions tunes the startup probe. Zero values use production defaults.
type ProbeOptions struct {
	// ReadyWait bounds how long a not-ready daemon (503 or no answer) is
	// waited for without any sign of startup progress. Zero uses
	// DefaultReadyWait.
	ReadyWait time.Duration
	// MaxReadyWait caps the bound after the daemon's advertised budget and
	// progress extend it. Zero uses DefaultMaxReadyWait; it never shortens
	// ReadyWait.
	MaxReadyWait time.Duration
	// BaseDelay and MaxDelay pace the jittered backoff between attempts.
	BaseDelay, MaxDelay time.Duration
	// Log receives INFO progress lines while waiting; nil discards them.
	Log io.Writer
}

// probePutter is the single-attempt write the probe classifies itself.
type probePutter interface {
	PutOnce(ctx context.Context, digest string, data []byte) error
}

// Resolve preserves GOOBERS_BLOB_ENDPOINT's stage-pod meaning when a directory
// is selected. An explicit endpoint always conflicts with a directory.
func Resolve(directory, endpoint, environment string, instanceBacked bool) (string, error) {
	if instanceBacked && endpoint == "" && directory == "" {
		endpoint = environment
	}
	if directory != "" && endpoint != "" || instanceBacked && directory == "" && endpoint == "" {
		return "", fmt.Errorf("WORKER_BLOB_MODE: select exactly one of --blob-store or --blob-endpoint")
	}
	if endpoint != "" {
		if err := ValidateEndpoint(endpoint); err != nil {
			return "", err
		}
	}
	return endpoint, nil
}

// ValidateEndpoint returns credential-free errors before endpoints reach logs.
func ValidateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return fmt.Errorf("WORKER_BLOB_ENDPOINT: requires an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("WORKER_BLOB_ENDPOINT: invalid TCP port")
		}
	}
	return nil
}

// Open needs worker authentication only when traffic crosses the blob plane.
func Open(ctx context.Context, directory, endpoint, dispatchEndpoint string, source func() (string, error), probe ProbeOptions) (blobstore.Store, error) {
	if directory == "" {
		if source == nil {
			return nil, fmt.Errorf("WORKER_BLOB_AUTH: api.podTokenKeyFile is required for endpoint mode")
		}
		return &dispatcher.BlobClient{BaseURL: endpoint, TokenSource: source}, nil
	}
	local, err := blobstore.NewDir(directory)
	if err != nil {
		return nil, err
	}
	if dispatchEndpoint != "" {
		if err := ValidateEndpoint(dispatchEndpoint); err != nil {
			return nil, err
		}
		if source == nil {
			return nil, fmt.Errorf("WORKER_BLOB_AUTH: api.podTokenKeyFile is required for dispatch preflight")
		}
		remote := &dispatcher.BlobClient{BaseURL: dispatchEndpoint, TokenSource: source}
		if err := VerifyShared(ctx, local, remote, probe); err != nil {
			return nil, err
		}
	}
	return local, nil
}

// VerifyShared proves a fresh plane write is visible through the worker mount.
// The tiny random content-addressed probe is intentionally retained as a blob.
//
// A daemon that is not ready yet (HTTP 503, or no answer at all) is waited for
// with backoff, logging progress, without failing: that is expected on every
// rollout. How long is readinessBound's call: opts.ReadyWait, moved by the
// startup budget and progress the daemon advertises on its 503. Any other
// refusal (4xx, a non-503 5xx such as a blob-plane 500) or a read-back
// mismatch fails immediately with
// WORKER_BLOB_STORE_MISMATCH, including the daemon's error body.
func VerifyShared(ctx context.Context, local blobstore.Store, remote probePutter, opts ProbeOptions) error {
	base, max := opts.BaseDelay, opts.MaxDelay
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	if max <= 0 {
		max = 15 * time.Second
	}
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return fmt.Errorf("WORKER_BLOB_STORE_MISMATCH: cannot generate probe")
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	bound := newReadinessBound(time.Now(), opts)
	start := bound.start
	attempts := 0
	var lastStatus *dispatcher.BlobStatusError
	var lastErr error
	err := retryutil.Until(ctx, bound.limit.Sub(start), retryutil.Policy{Base: base, Max: max}, func(ctx context.Context) (bool, error) {
		if lastErr != nil && !time.Now().Before(bound.deadline) {
			return false, lastErr
		}
		attempts++
		putCtx, cancel := context.WithDeadline(ctx, earliest(time.Now().Add(30*time.Second), bound.deadline))
		defer cancel()
		err := remote.PutOnce(putCtx, digest, data)
		if err == nil {
			return false, nil
		}
		if !notReady(err) {
			return false, fmt.Errorf("WORKER_BLOB_STORE_MISMATCH: blob-plane probe PUT failed: %w", err)
		}
		var hints startuphint.Hints
		var status *dispatcher.BlobStatusError
		if errors.As(err, &status) {
			lastStatus = status
			hints = startuphint.Parse(status.Header)
		}
		now := time.Now()
		bound.observe(now, hints)
		lastErr = err
		if opts.Log != nil {
			_, _ = fmt.Fprintf(opts.Log, "INFO goobers worker: blob-plane probe waiting for daemon readiness (attempt %d, %s elapsed, bound %s%s): %v\n",
				attempts, now.Sub(start).Round(time.Second), bound.deadline.Sub(start).Round(time.Second), describeHints(hints), err)
		}
		return now.Before(bound.deadline), err
	})
	if err != nil {
		if strings.HasPrefix(err.Error(), "WORKER_BLOB_STORE_MISMATCH") {
			return err
		}
		// The readiness bound can cancel an attempt mid-flight, leaving a bare
		// "context deadline exceeded" as the final error. Keep the daemon's
		// last actual answer: it is the only part that says why it was not ready.
		if lastStatus != nil && !strings.Contains(err.Error(), lastStatus.Error()) {
			return fmt.Errorf("WORKER_DAEMON_NOT_READY: daemon blob plane not ready after %s: %w (last daemon response: %w)", time.Since(start).Round(time.Second), err, lastStatus)
		}
		return fmt.Errorf("WORKER_DAEMON_NOT_READY: daemon blob plane not ready after %s: %w", time.Since(start).Round(time.Second), err)
	}
	got, err := local.Get(ctx, digest)
	if err != nil || !bytes.Equal(got, data) {
		return fmt.Errorf("WORKER_BLOB_STORE_MISMATCH: daemon probe is absent from --blob-store; mount the daemon store or use --blob-endpoint")
	}
	return nil
}

// readinessBound is when the probe stops waiting for a not-ready daemon.
//
// It starts at ReadyWait. A daemon that advertises its startup budget moves
// it to the end of that budget plus a grace, and a daemon whose progress
// token changes earns a fresh ReadyWait from that moment: a slow daemon that
// is visibly advancing is waited for, while one that stops advancing is given
// up on ReadyWait after its last progress (#6895). Neither can move it past
// limit.
type readinessBound struct {
	start, deadline, limit time.Time
	stall                  time.Duration
	progress               string
}

func newReadinessBound(now time.Time, opts ProbeOptions) *readinessBound {
	wait := opts.ReadyWait
	if wait <= 0 {
		wait = DefaultReadyWait
	}
	maxWait := opts.MaxReadyWait
	if maxWait <= 0 {
		maxWait = DefaultMaxReadyWait
	}
	if maxWait < wait {
		maxWait = wait
	}
	return &readinessBound{start: now, deadline: now.Add(wait), limit: now.Add(maxWait), stall: wait}
}

// observe folds one not-ready answer's hints into the bound. The first
// progress token seen is a baseline, not progress. A spent budget extends
// nothing, so a daemon past its own estimate is held only by real progress.
func (b *readinessBound) observe(now time.Time, hints startuphint.Hints) {
	next := b.deadline
	if hints.HasBudget && hints.BudgetRemaining > 0 {
		next = latest(next, now.Add(hints.BudgetRemaining+startupBudgetGrace))
	}
	if hints.Progress != "" && hints.Progress != b.progress {
		if b.progress != "" {
			next = latest(next, now.Add(b.stall))
		}
		b.progress = hints.Progress
	}
	b.deadline = earliest(next, b.limit)
}

func describeHints(hints startuphint.Hints) string {
	var parts []string
	if hints.HasBudget {
		parts = append(parts, fmt.Sprintf("daemon startup budget remaining %s", hints.BudgetRemaining))
	}
	if hints.Progress != "" {
		parts = append(parts, "daemon progress "+hints.Progress)
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, ", ")
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// notReady reports a daemon that has not finished starting: an HTTP 503 or a
// transport failure before any answer. Everything else is a real failure.
func notReady(err error) bool {
	var status *dispatcher.BlobStatusError
	if errors.As(err, &status) {
		return status.StatusCode == http.StatusServiceUnavailable
	}
	var transport *dispatcher.BlobTransportError
	return errors.As(err, &transport)
}
