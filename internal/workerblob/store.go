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
)

// DefaultReadyWait bounds how long the startup probe waits for a daemon that
// is still starting. A rollout takes the daemon 4-9 minutes to reach ready
// (#6649); 20 minutes leaves generous headroom before a worker concludes the
// daemon is down and exits.
const DefaultReadyWait = 20 * time.Minute

// ProbeOptions tunes the startup probe. Zero values use production defaults.
type ProbeOptions struct {
	// ReadyWait bounds how long a not-ready daemon (503 or no answer) is
	// waited for. Zero uses DefaultReadyWait.
	ReadyWait time.Duration
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
// with backoff up to opts.ReadyWait, logging progress, without failing: that is
// expected on every rollout. Any other refusal (4xx, a non-503 5xx such as a
// blob-plane 500) or a read-back mismatch fails immediately with
// WORKER_BLOB_STORE_MISMATCH, including the daemon's error body.
func VerifyShared(ctx context.Context, local blobstore.Store, remote probePutter, opts ProbeOptions) error {
	wait := opts.ReadyWait
	if wait <= 0 {
		wait = DefaultReadyWait
	}
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
	start := time.Now()
	attempts := 0
	err := retryutil.Until(ctx, wait, retryutil.Policy{Base: base, Max: max}, func(ctx context.Context) (bool, error) {
		attempts++
		putCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err := remote.PutOnce(putCtx, digest, data)
		if err == nil {
			return false, nil
		}
		if notReady(err) {
			if opts.Log != nil {
				_, _ = fmt.Fprintf(opts.Log, "INFO goobers worker: blob-plane probe waiting for daemon readiness (attempt %d, %s elapsed, bound %s): %v\n", attempts, time.Since(start).Round(time.Second), wait, err)
			}
			return true, err
		}
		return false, fmt.Errorf("WORKER_BLOB_STORE_MISMATCH: blob-plane probe PUT failed: %w", err)
	})
	if err != nil {
		if strings.HasPrefix(err.Error(), "WORKER_BLOB_STORE_MISMATCH") {
			return err
		}
		return fmt.Errorf("WORKER_DAEMON_NOT_READY: daemon blob plane not ready after %s: %w", time.Since(start).Round(time.Second), err)
	}
	got, err := local.Get(ctx, digest)
	if err != nil || !bytes.Equal(got, data) {
		return fmt.Errorf("WORKER_BLOB_STORE_MISMATCH: daemon probe is absent from --blob-store; mount the daemon store or use --blob-endpoint")
	}
	return nil
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
