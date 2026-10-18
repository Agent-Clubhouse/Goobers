package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/retryutil"
)

const remoteRootFixture = `{"instanceRoot":"/remote/root","rootIdentity":{"id":"0123456789abcdef0123456789abcdef"}}`

func serveRemoteRootFixture(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != apicontract.InstancePath {
		return false
	}
	_, _ = fmt.Fprint(w, remoteRootFixture)
	return true
}

func TestRemoteRootRefusesUnverifiableIdentity(t *testing.T) {
	for name, body := range map[string]string{
		"legacy":              `{"instanceRoot":"/remote/root"}`,
		"malformed":           `null`,
		"bad id":              strings.Replace(remoteRootFixture, "0123456789abcdef0123456789abcdef", "invalid", 1),
		"historical":          strings.Replace(remoteRootFixture, `"id":`, `"decommissionReason":"migrated","id":`, 1),
		"lifecycle unknown":   strings.Replace(remoteRootFixture, `"id":`, `"lifecycleProblem":"unreadable","id":`, 1),
		"trailing":            remoteRootFixture + `{}`,
		"oversized":           remoteRootFixture + strings.Repeat(" ", maxRemoteTriggerResponseBody),
		"duplicate id":        strings.Replace(remoteRootFixture, `"id":`, `"id":"other","id":`, 1),
		"duplicate lifecycle": strings.Replace(remoteRootFixture, `"id":`, `"lifecycleProblem":"broken","lifecycleProblem":"","id":`, 1),
		"case alias":          strings.Replace(remoteRootFixture, `"rootIdentity":`, `"RootIdentity":{},"rootIdentity":`, 1),
		"case id":             strings.Replace(remoteRootFixture, `"id":`, `"ID":`, 1),
		"invalid utf8":        strings.Replace(remoteRootFixture, "/remote/root", "/remote/"+string([]byte{0xff}), 1),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("mutation sent without valid identity")
				}
				_, _ = fmt.Fprint(w, body)
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			if code := runRemoteTrigger(context.Background(), server.URL, runTarget{Workflow: "test"}, "test", true, remoteTriggerTimeout, &stdout, &stderr); code != 2 {
				t.Fatalf("invalid target accepted: %d %s", code, stderr.String())
			}
		})
	}
}

func TestRemoteRootAuthenticatedDisplayBeforeMutation(t *testing.T) {
	t.Setenv("GOOBERS_API_TOKEN", "test-token")
	var stdout, stderr bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing authorization")
		}
		if serveRemoteRootFixture(w, r) {
			return
		}
		if !strings.Contains(stderr.String(), `Remote instance root: "/remote/root"; instance ID: "0123456789abcdef0123456789abcdef"`) {
			t.Error("mutation preceded identity display")
		}
		_, _ = fmt.Fprint(w, `{"runId":"test"}`)
	}))
	defer server.Close()
	if code := runRemoteTrigger(context.Background(), server.URL, runTarget{Workflow: "test"}, "test", true, remoteTriggerTimeout, &stdout, &stderr); code != 0 {
		t.Fatalf("trigger failed: %d %s", code, stderr.String())
	}
	if err := prepareRemoteRoot(context.Background(), server.URL, brokenRootBannerWriter{}); err == nil {
		t.Fatal("broken identity display accepted")
	}
}

func TestRemoteRootRejectsRedirectAtEitherStep(t *testing.T) {
	for _, redirectIdentity := range []bool{true, false} {
		t.Run(fmt.Sprintf("identity=%t", redirectIdentity), func(t *testing.T) {
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("followed redirect to an unidentified daemon")
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !redirectIdentity && serveRemoteRootFixture(w, r) {
					return
				}
				http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			if code := runRemoteTrigger(context.Background(), server.URL, runTarget{Workflow: "test"}, "test", true, remoteTriggerTimeout, &stdout, &stderr); code != 2 {
				t.Fatalf("redirect accepted: %d %s", code, stderr.String())
			}
		})
	}
}

func TestRemoteManualCommandsRequireIdentityBeforeMutation(t *testing.T) {
	commands := [][]string{
		{"run", "cancel", "run-1"},
		{"run", "abort", "run-1"},
		{"approve", "--actor=ops", "run-1", "gate"},
		{"override", "--actor=ops", "--rationale=test", "run-1", "gate"},
		{"rerun-stage", "--actor=ops", "--addendum=test", "run-1", "gate"},
		{"escalations", "resolve", "--actor=ops", "--resolution=approve", "--gate=gate", "run-1"},
	}
	for _, args := range commands {
		t.Run(strings.Join(args[:2], "-"), func(t *testing.T) {
			var reads, writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == apicontract.InstancePath {
					reads.Add(1)
					_, _ = fmt.Fprint(w, `{"instanceRoot":"/legacy"}`)
					return
				}
				writes.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			t.Setenv(remoteDaemonAPIEnv, server.URL)
			code, _, stderr := runArgs(t, args...)
			if code != 2 || reads.Load() != 1 || writes.Load() != 0 || !strings.Contains(stderr, "durable root identity") {
				t.Fatalf("identity guard bypassed: code=%d reads=%d writes=%d stderr=%s", code, reads.Load(), writes.Load(), stderr)
			}
		})
	}
}

// restartingDaemon answers identity reads the way a daemon does right after a
// restart (#5897): the startup placeholder for the first `starting` reads, the
// API's crash-recovery gate for the next `recovering` reads, then a ready
// identity. Both not-ready answers come from the daemon's own handlers, so a
// change to either one that the CLI stops recognizing fails these tests.
type restartingDaemon struct {
	starting, recovering int32
	recoveryGate         http.Handler
	mutation             http.HandlerFunc
	reads, writes        atomic.Int32
}

func newRestartingDaemon(t *testing.T, starting, recovering int32, mutation http.HandlerFunc) *restartingDaemon {
	t.Helper()
	gate, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.AllowAll, log.New(io.Discard, "", 0),
		httpapi.WithRecoveryGate(func() bool { return false }))
	if err != nil {
		t.Fatal(err)
	}
	return &restartingDaemon{starting: starting, recovering: recovering, recoveryGate: gate, mutation: mutation}
}

func (d *restartingDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != apicontract.InstancePath {
		d.writes.Add(1)
		d.mutation(w, r)
		return
	}
	switch read := d.reads.Add(1); {
	case read <= d.starting:
		serveDaemonStarting(w, r)
	case read <= d.starting+d.recovering:
		d.recoveryGate.ServeHTTP(w, r)
	default:
		_, _ = fmt.Fprint(w, remoteRootFixture)
	}
}

func acceptTrigger(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = fmt.Fprint(w, `{"acceptanceId":"acceptance-1","state":"pending"}`)
}

func refuseMutation(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprint(w, `{"error":{"code":"run_not_found","message":"no such run"}}`)
}

// fastRemoteRootWait keeps a test's re-reads milliseconds apart.
func fastRemoteRootWait() remoteRootWait {
	return remoteRootWait{fallback: time.Minute, policy: retryutil.Policy{Base: time.Millisecond, Max: 2 * time.Millisecond}}
}

func TestStartupPlaceholderIsARetryableNotReadyAnswer(t *testing.T) {
	response := httptest.NewRecorder()
	serveDaemonStarting(response, httptest.NewRequest(http.MethodGet, apicontract.InstancePath, nil))
	if response.Code != http.StatusServiceUnavailable ||
		response.Header().Get("Retry-After") != strconv.Itoa(httpapi.NotReadyRetryAfterSeconds) {
		t.Fatalf("placeholder = %d Retry-After=%q, want 503 with a Retry-After hint", response.Code, response.Header().Get("Retry-After"))
	}
	// The literal text is the wire contract: a newer CLI recognizes an older
	// daemon's placeholder by it, so it must not change.
	if body := response.Body.String(); body != "daemon is starting\n" || daemonNotReady([]byte(body)) == nil {
		t.Fatalf("placeholder body = %q, want the unchanged text the CLI recognizes", body)
	}
}

func TestRemoteRootWaitsOutDaemonRestart(t *testing.T) {
	daemon := newRestartingDaemon(t, 2, 2, acceptTrigger)
	server := httptest.NewServer(daemon)
	defer server.Close()
	var diagnostic bytes.Buffer
	if err := prepareRemoteRootWithWait(context.Background(), server.URL, "", &diagnostic, fastRemoteRootWait()); err != nil {
		t.Fatalf("restarting daemon was not waited out: %v\n%s", err, diagnostic.String())
	}
	if got := daemon.reads.Load(); got != 5 {
		t.Fatalf("identity reads = %d, want 5 (2 starting, 2 recovering, 1 ready)", got)
	}
	output := diagnostic.String()
	if strings.Count(output, "note: ") != 1 ||
		!strings.Contains(output, "note: daemon is still starting, which is normal right after a restart; waiting up to 1m0s") {
		t.Fatalf("diagnostic = %q, want exactly one waiting note naming the phase and the limit", output)
	}
	if !strings.Contains(output, `Remote instance root: "/remote/root"`) {
		t.Fatalf("diagnostic = %q, want the identity banner once the daemon is ready", output)
	}
}

func TestRemoteRootReportsDaemonStillStartingWhenWaitEnds(t *testing.T) {
	for name, phases := range map[string]struct {
		starting, recovering int32
		want                 string
	}{
		"startup placeholder": {starting: 1 << 20, want: "daemon is still starting after waiting"},
		"crash recovery":      {recovering: 1 << 20, want: "daemon is still completing crash recovery after waiting"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			daemon := newRestartingDaemon(t, phases.starting, phases.recovering, acceptTrigger)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				daemon.ServeHTTP(w, r)
				if daemon.reads.Load() == 3 {
					cancel() // the caller's time runs out while the daemon is still not ready
				}
			}))
			defer server.Close()
			var diagnostic bytes.Buffer
			err := prepareRemoteRootWithWait(ctx, server.URL, "", &diagnostic, fastRemoteRootWait())
			if err == nil || !strings.Contains(err.Error(), phases.want) ||
				!strings.Contains(err.Error(), "no mutation sent; this is normal right after a restart, so retry shortly") {
				t.Fatalf("err = %v, want %q and a retry hint", err, phases.want)
			}
			if strings.Contains(err.Error(), "root identity unavailable") {
				t.Fatalf("err = %v, still reads like a broken identity", err)
			}
			if got := daemon.reads.Load(); got != 3 || strings.Count(diagnostic.String(), "note: ") != 1 {
				t.Fatalf("reads = %d diagnostic = %q, want 3 reads and one waiting note", got, diagnostic.String())
			}
		})
	}
}

func TestRemoteRootReportsOtherFailuresWithoutWaiting(t *testing.T) {
	jsonError := func(code string) string { return fmt.Sprintf(`{"error":{"code":%q,"message":"refused"}}`, code) }
	for name, answer := range map[string]struct {
		status int
		body   string
	}{
		"server error":        {http.StatusInternalServerError, "boom"},
		"request budget shed": {http.StatusServiceUnavailable, jsonError("request_budget_exceeded")},
		"unrecognized 503":    {http.StatusServiceUnavailable, "maintenance window"},
		"unauthenticated":     {http.StatusUnauthorized, jsonError("unauthenticated")},
		"not found":           {http.StatusNotFound, "404 page not found"},
	} {
		t.Run(name, func(t *testing.T) {
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reads.Add(1)
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(answer.status)
				_, _ = fmt.Fprint(w, answer.body)
			}))
			defer server.Close()
			var diagnostic bytes.Buffer
			err := prepareRemoteRootWithWait(context.Background(), server.URL, "", &diagnostic, fastRemoteRootWait())
			want := fmt.Sprintf("daemon root identity unavailable (HTTP %d); no mutation sent", answer.status)
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if reads.Load() != 1 || strings.Contains(diagnostic.String(), "note: ") {
				t.Fatalf("reads = %d diagnostic = %q; only a restarting daemon is waited on", reads.Load(), diagnostic.String())
			}
		})
	}
}

func TestRemoteRootWaitLimitFollowsCallerDeadline(t *testing.T) {
	if got := remoteRootWaitLimit(context.Background(), 30*time.Second); got != 30*time.Second {
		t.Fatalf("without a caller deadline: limit = %s, want the 30s fallback", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if got := remoteRootWaitLimit(ctx, 30*time.Second); got <= 9*time.Minute || got > 10*time.Minute {
		t.Fatalf("with a 10m caller deadline: limit = %s, want the caller's remaining time", got)
	}
}

// The tests below use the shipped pacing: one re-read, at most 250ms later.

func TestRunWaitsForRestartingDaemonBeforeSubmitting(t *testing.T) {
	daemon := newRestartingDaemon(t, 0, 1, acceptTrigger)
	server := httptest.NewServer(daemon)
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := runRemoteTrigger(context.Background(), server.URL, runTarget{Workflow: "test"}, "test", true, remoteTriggerTimeout, &stdout, &stderr)
	if code != 0 || daemon.reads.Load() != 2 || daemon.writes.Load() != 1 {
		t.Fatalf("code=%d reads=%d writes=%d stderr=%s", code, daemon.reads.Load(), daemon.writes.Load(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "note: daemon is still completing crash recovery, which is normal right after a restart; waiting up to 30s") {
		t.Fatalf("stderr = %q, want a waiting note bounded by --api-timeout", stderr.String())
	}
	if !strings.Contains(stdout.String(), "accepted trigger acceptance-1") {
		t.Fatalf("stdout = %q, want the accepted trigger", stdout.String())
	}
}

func TestManualCommandsWaitForRestartingDaemon(t *testing.T) {
	for _, args := range [][]string{
		{"run", "cancel", "run-1"},
		{"approve", "--actor=ops", "run-1", "gate"},
	} {
		t.Run(strings.Join(args[:2], "-"), func(t *testing.T) {
			daemon := newRestartingDaemon(t, 1, 0, refuseMutation)
			server := httptest.NewServer(daemon)
			defer server.Close()
			t.Setenv(remoteDaemonAPIEnv, server.URL)
			_, _, stderr := runArgs(t, args...)
			if daemon.reads.Load() != 2 || daemon.writes.Load() != 1 {
				t.Fatalf("reads=%d writes=%d stderr=%s; want startup waited out, then the request sent once",
					daemon.reads.Load(), daemon.writes.Load(), stderr)
			}
			if !strings.Contains(stderr, "note: daemon is still starting, which is normal right after a restart; waiting up to 30s") {
				t.Fatalf("stderr = %q, want a waiting note bounded by the 30s fallback", stderr)
			}
		})
	}
}
