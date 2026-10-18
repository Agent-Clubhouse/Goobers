package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/retryutil"
)

// maxDaemonNotReadyBody bounds how much of a 503 body is read to recognize a
// daemon that is still starting; both recognized answers are a few bytes.
const maxDaemonNotReadyBody = 4 << 10

// remoteRootWait bounds and paces identity re-reads while a restarting daemon
// answers that it has not finished starting (#5897).
type remoteRootWait struct {
	// fallback bounds the wait when the caller's context has no deadline of
	// its own: every mutation command except `goobers run`, whose deadline is
	// --api-timeout.
	fallback time.Duration
	policy   retryutil.Policy
}

// defaultRemoteRootWait waits as long as `goobers run`'s default
// --api-timeout. It re-reads quickly at first, then never waits longer between
// reads than the Retry-After hint the daemon sends, so a daemon that becomes
// ready is noticed within a couple of seconds.
var defaultRemoteRootWait = remoteRootWait{
	fallback: remoteTriggerTimeout,
	policy:   retryutil.Policy{Base: 250 * time.Millisecond, Max: httpapi.NotReadyRetryAfterSeconds * time.Second},
}

// daemonNotReadyError is a daemon answering that it has not finished starting.
// Unlike every other identity failure it is expected to clear on its own.
type daemonNotReadyError struct {
	phase string
}

func (e *daemonNotReadyError) Error() string { return "daemon is " + e.phase }

// prepareRemoteRoot reads the target daemon, never the client's filesystem.
// Older daemons without durable identity must be upgraded before manual writes.
// Redirects are refused: a different server cannot vouch for this target.
func prepareRemoteRoot(ctx context.Context, endpoint string, diagnostic io.Writer) error {
	return prepareRemoteRootForInstance(ctx, endpoint, "", diagnostic)
}

func prepareRemoteRootForInstance(ctx context.Context, endpoint, expectedID string, diagnostic io.Writer) error {
	return prepareRemoteRootWithWait(ctx, endpoint, expectedID, diagnostic, defaultRemoteRootWait)
}

func prepareRemoteRootWithWait(ctx context.Context, endpoint, expectedID string, diagnostic io.Writer, wait remoteRootWait) error {
	root, identity, err := awaitRemoteRoot(ctx, endpoint, diagnostic, wait)
	if err != nil {
		return err
	}
	if err := validateRemoteRoot(root, identity); err != nil {
		return err
	}
	if expectedID != "" && identity.ID != expectedID {
		return fmt.Errorf("discovered daemon belongs to another instance; no mutation sent")
	}
	if _, err := fmt.Fprintf(diagnostic, "Remote instance root: %q; instance ID: %q\n", root, identity.ID); err != nil {
		return fmt.Errorf("display remote mutation target: %w", err)
	}
	return nil
}

// awaitRemoteRoot reads the daemon's root identity. A daemon that answers it
// is still starting or still completing crash recovery is re-read until the
// caller's deadline, or wait.fallback without one: that answer is expected for
// a while after every restart. Any other answer, success or failure, returns
// from the first read exactly as it did before the wait existed.
func awaitRemoteRoot(ctx context.Context, endpoint string, diagnostic io.Writer, wait remoteRootWait) (string, *readservice.RootIdentity, error) {
	limit := remoteRootWaitLimit(ctx, wait.fallback)
	started := time.Now()
	var (
		root     string
		identity *readservice.RootIdentity
		last     *daemonNotReadyError
	)
	err := retryutil.Until(ctx, limit, wait.policy, func(ctx context.Context) (bool, error) {
		var err error
		root, identity, err = readRemoteRoot(ctx, endpoint)
		var notReady *daemonNotReadyError
		switch {
		case errors.As(err, &notReady):
			if last == nil {
				pf(diagnostic, "note: %v, which is normal right after a restart; waiting up to %s for it to become ready\n",
					notReady, limit.Round(time.Second))
			}
			last = notReady
			return true, err
		case err != nil && last != nil && ctx.Err() != nil:
			// The wait ran out during a read: report the daemon's last
			// answer, not the abandoned call.
			return true, last
		default:
			return false, err
		}
	})
	if errors.As(err, &last) {
		return "", nil, fmt.Errorf("%w after waiting %s; no mutation sent; this is normal right after a restart, so retry shortly",
			last, time.Since(started).Round(time.Second))
	}
	return root, identity, err
}

// remoteRootWaitLimit is how long to wait for a restarting daemon: until the
// caller's own deadline when it has one, otherwise fallback.
func remoteRootWaitLimit(ctx context.Context, fallback time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return time.Until(deadline)
	}
	return fallback
}

// readRemoteRoot performs one identity read.
func readRemoteRoot(ctx context.Context, endpoint string) (string, *readservice.RootIdentity, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+apicontract.InstancePath, nil)
	if err != nil {
		return "", nil, fmt.Errorf("build daemon identity request")
	}
	request.Header.Set("Accept", "application/json")
	if token := strings.TrimSpace(os.Getenv("GOOBERS_API_TOKEN")); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: remoteTriggerTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		// Transport errors may contain credential-bearing URLs. Do not echo them.
		return "", nil, fmt.Errorf("call daemon API for root identity failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", nil, remoteRootRefusal(response)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRemoteTriggerResponseBody+1))
	if err != nil || len(body) > maxRemoteTriggerResponseBody {
		return "", nil, fmt.Errorf("daemon root identity response unreadable or oversized")
	}
	return decodeRemoteRoot(body)
}

// remoteRootRefusal classifies a non-200 identity answer: *daemonNotReadyError
// for a daemon that has not finished starting, the original error otherwise.
func remoteRootRefusal(response *http.Response) error {
	if response.StatusCode == http.StatusServiceUnavailable {
		body, err := io.ReadAll(io.LimitReader(response.Body, maxDaemonNotReadyBody))
		if err == nil {
			if notReady := daemonNotReady(body); notReady != nil {
				return notReady
			}
		}
	}
	return fmt.Errorf("daemon root identity unavailable (HTTP %d); no mutation sent", response.StatusCode)
}

// daemonNotReady recognizes the two 503 bodies a restarting daemon answers
// with before it can serve identity: the plain-text startup placeholder that
// answers until the full API is installed (startup_api.go), and the API's
// crash-recovery gate (httpapi.CodeRecovering). Any other 503 is something
// else and is reported at once.
func daemonNotReady(body []byte) *daemonNotReadyError {
	var envelope apicontract.ErrorEnvelope
	if json.Unmarshal(body, &envelope) == nil {
		if envelope.Error.Code == httpapi.CodeRecovering {
			return &daemonNotReadyError{phase: "still completing crash recovery"}
		}
		return nil
	}
	if strings.TrimSpace(string(body)) == daemonStartingMessage {
		return &daemonNotReadyError{phase: "still starting"}
	}
	return nil
}

func validateRemoteRoot(root string, identity *readservice.RootIdentity) error {
	if strings.TrimSpace(root) == "" || len(root) > 4096 || identity == nil {
		return fmt.Errorf("daemon has no usable durable root identity; upgrade or repair it before mutation")
	}
	id, err := hex.DecodeString(identity.ID)
	if err != nil || len(id) != 16 || strings.ToLower(identity.ID) != identity.ID || identity.IdentityProblem != "" {
		return fmt.Errorf("daemon durable root identity is unavailable or invalid")
	}
	if identity.DecommissionedAt != nil || identity.DecommissionReason != "" || identity.LifecycleProblem != "" {
		return fmt.Errorf("daemon root is historical or its lifecycle is unverifiable; no mutation sent")
	}
	return nil
}
