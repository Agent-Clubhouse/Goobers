package workerblob

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/startuphint"
)

// dirPutter writes straight into a directory store, standing in for a daemon
// sharing (or not sharing) the worker's mount.
type dirPutter struct{ store blobstore.Store }

func (d dirPutter) PutOnce(ctx context.Context, digest string, data []byte) error {
	return d.store.Put(ctx, digest, data)
}

type probePutterFunc func(context.Context, string, []byte) error

func (f probePutterFunc) PutOnce(ctx context.Context, digest string, data []byte) error {
	return f(ctx, digest, data)
}

var fastProbe = ProbeOptions{ReadyWait: 5 * time.Second, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}

// probeServer answers the first notReady PUTs with status/body, then 200 after
// writing into store.
func probeServer(t *testing.T, store blobstore.Store, notReady int32, status int, body string) (*dispatcher.BlobClient, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= notReady {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		digest := strings.TrimPrefix(r.URL.Path, dispatcher.BlobPathPrefix)
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		if err := store.Put(r.Context(), digest, buf.Bytes()); err != nil {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return &dispatcher.BlobClient{BaseURL: srv.URL}, &calls
}

func TestProbeWaitsThroughNotReady503(t *testing.T) {
	local, _ := blobstore.NewDir(t.TempDir())
	remote, calls := probeServer(t, local, 4, http.StatusServiceUnavailable, "starting")
	var log bytes.Buffer
	opts := fastProbe
	opts.Log = &log
	if err := VerifyShared(context.Background(), local, remote, opts); err != nil {
		t.Fatalf("503 must be waited out: %v", err)
	}
	if calls.Load() != 5 || strings.Count(log.String(), "INFO") != 4 {
		t.Fatalf("calls=%d log=%q", calls.Load(), log.String())
	}
}

func TestProbeWaitsThroughConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	opts := fastProbe
	opts.ReadyWait = 100 * time.Millisecond
	err := VerifyShared(context.Background(), nil, &dispatcher.BlobClient{BaseURL: url}, opts)
	if err == nil || !strings.Contains(err.Error(), "WORKER_DAEMON_NOT_READY") || strings.Contains(err.Error(), "WORKER_BLOB_STORE_MISMATCH") {
		t.Fatalf("unreachable daemon past the bound must be not-ready, not mismatch: %v", err)
	}
}

func TestProbeNotReadyBoundExpires(t *testing.T) {
	local, _ := blobstore.NewDir(t.TempDir())
	var calls atomic.Int32
	remote := probePutterFunc(func(context.Context, string, []byte) error {
		calls.Add(1)
		return &dispatcher.BlobTransportError{Op: "put", Err: errors.New("connection refused")}
	})
	opts := fastProbe
	opts.ReadyWait = 50 * time.Millisecond
	err := VerifyShared(context.Background(), local, remote, opts)
	if err == nil || !strings.Contains(err.Error(), "WORKER_DAEMON_NOT_READY") || strings.Contains(err.Error(), "WORKER_BLOB_STORE_MISMATCH") {
		t.Fatalf("an unanswered probe that outlasts the readiness bound must be not-ready: %v", err)
	}
	if calls.Load() == 0 {
		t.Fatal("expected at least one unanswered probe")
	}
}

func TestProbeNotReadyBoundExpiresPreservesLastStatus(t *testing.T) {
	local, _ := blobstore.NewDir(t.TempDir())
	remote := probePutterFunc(func(context.Context, string, []byte) error {
		return &dispatcher.BlobStatusError{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Body:       "still starting",
		}
	})
	opts := fastProbe
	opts.ReadyWait = 50 * time.Millisecond
	err := VerifyShared(context.Background(), local, remote, opts)
	if err == nil || !strings.Contains(err.Error(), "WORKER_DAEMON_NOT_READY") || !strings.Contains(err.Error(), "still starting") {
		t.Fatalf("got %v", err)
	}
}

func TestProbeFailsFastOnRealFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{500, "chmod: operation not permitted (CIFS)"}, {502, "bad gateway"}, {401, "unauthorized"}, {403, "forbidden"}, {404, "nope"}} {
		local, _ := blobstore.NewDir(t.TempDir())
		remote, calls := probeServer(t, local, 1<<30, tc.status, tc.body)
		err := VerifyShared(context.Background(), local, remote, fastProbe)
		if err == nil || !strings.Contains(err.Error(), "WORKER_BLOB_STORE_MISMATCH") || !strings.Contains(err.Error(), tc.body) {
			t.Fatalf("%d: got %v", tc.status, err)
		}
		if calls.Load() != 1 {
			t.Fatalf("%d: retried %d times", tc.status, calls.Load())
		}
	}
}

func TestProbeReadBackMismatchFailsFast(t *testing.T) {
	local, _ := blobstore.NewDir(t.TempDir())
	other, _ := blobstore.NewDir(t.TempDir())
	remote, _ := probeServer(t, other, 0, 0, "")
	err := VerifyShared(context.Background(), local, remote, fastProbe)
	if err == nil || !strings.Contains(err.Error(), "WORKER_BLOB_STORE_MISMATCH") || !strings.Contains(err.Error(), "absent from --blob-store") {
		t.Fatalf("got %v", err)
	}
}

func TestProbeContextCancelStopsWait(t *testing.T) {
	local, _ := blobstore.NewDir(t.TempDir())
	remote, _ := probeServer(t, local, 1<<30, http.StatusServiceUnavailable, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	opts := fastProbe
	opts.ReadyWait = time.Hour
	if err := VerifyShared(ctx, local, remote, opts); err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestResolveModes(t *testing.T) {
	for _, tt := range []struct {
		dir, endpoint, env string
		instance, refuse   bool
		want               string
	}{
		{"", "", "", true, true, ""}, {"dir", "http://plane", "", true, true, ""},
		{"dir", "", "http://stage-plane", true, false, ""},
		{"", "", "http://plane", true, false, "http://plane"},
		{"", "http://explicit", "http://default", true, false, "http://explicit"},
		{"", "", "", false, false, ""},
		{"", "", "https://stage-only.invalid?unused", false, false, ""},
	} {
		got, err := Resolve(tt.dir, tt.endpoint, tt.env, tt.instance)
		if (err != nil) != tt.refuse || got != tt.want {
			t.Fatalf("resolve %+v = %q %v", tt, got, err)
		}
	}
	for _, raw := range []string{"https://user:secret@host", "https://host?secret=x", "https://host/#secret", "file:///secret", "http://host:99999"} {
		if err := ValidateEndpoint(raw); err == nil || strings.Contains(err.Error(), raw) {
			t.Fatal("unsafe endpoint accepted or echoed")
		}
	}
}

func TestDispatchDirectoryProbeRejectsDifferentStore(t *testing.T) {
	local, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyShared(context.Background(), local, dirPutter{other}, fastProbe); err == nil || !strings.Contains(err.Error(), "WORKER_BLOB_STORE_MISMATCH") {
		t.Fatalf("different directory accepted: %v", err)
	}
	if err := VerifyShared(context.Background(), local, dirPutter{local}, fastProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), t.TempDir(), "", "", nil, ProbeOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), "", "http://plane", "", nil, ProbeOptions{}); err == nil {
		t.Fatal("unsigned endpoint accepted")
	}
}

func TestReadinessBoundFollowsDaemonBudgetAndProgress(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	wait := 20 * time.Minute
	b := newReadinessBound(start, ProbeOptions{ReadyWait: wait, MaxReadyWait: 3 * time.Hour})
	at := func(d time.Duration) time.Time { return start.Add(d) }
	expect := func(step string, want time.Time) {
		t.Helper()
		if !b.deadline.Equal(want) {
			t.Fatalf("%s: deadline = %s after start, want %s", step, b.deadline.Sub(start), want.Sub(start))
		}
	}

	expect("silent daemon", at(wait))
	b.observe(at(time.Minute), startuphint.Hints{})
	expect("no hints", at(wait))

	// A daemon whose own budget outlasts the default is waited for until that
	// budget ends, plus grace.
	b.observe(at(time.Minute), startuphint.Hints{HasBudget: true, BudgetRemaining: 40 * time.Minute, Progress: "3"})
	expect("budget", at(time.Minute+40*time.Minute+startupBudgetGrace))
	// The first token is a baseline; repeating it is not progress.
	b.observe(at(30*time.Minute), startuphint.Hints{HasBudget: true, BudgetRemaining: 11 * time.Minute, Progress: "3"})
	expect("same token, shrinking budget", at(46*time.Minute))
	// A spent budget extends nothing on its own.
	b.observe(at(42*time.Minute), startuphint.Hints{HasBudget: true, Progress: "3"})
	expect("spent budget", at(46*time.Minute))
	// Progress past the budget earns a fresh ReadyWait from that moment.
	b.observe(at(45*time.Minute), startuphint.Hints{HasBudget: true, Progress: "4"})
	expect("progress", at(45*time.Minute+wait))
	// Nothing moves the bound past the cap.
	b.observe(at(2*time.Hour+50*time.Minute), startuphint.Hints{Progress: "5"})
	expect("cap", at(3*time.Hour))
}

func TestReadinessBoundIgnoresRestartedDaemonsResetProgressAndFreshBudget(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	wait := 20 * time.Minute
	b := newReadinessBound(start, ProbeOptions{ReadyWait: wait, MaxReadyWait: 6 * time.Hour})
	at := func(d time.Duration) time.Time { return start.Add(d) }

	b.observe(at(0), startuphint.Hints{HasBudget: true, BudgetRemaining: 10 * time.Minute, Progress: "17", Daemon: "a"})
	if !b.deadline.Equal(at(wait)) {
		t.Fatalf("first daemon: deadline = %s, want %s", b.deadline.Sub(start), wait)
	}
	// Each restart resets the counter and advertises a fresh full budget;
	// neither may read as the daemon advancing.
	for i, daemon := range []string{"b", "c", "d"} {
		now := at(time.Duration(i+1) * 5 * time.Minute)
		b.observe(now, startuphint.Hints{HasBudget: true, BudgetRemaining: time.Hour, Progress: "3", Daemon: daemon})
		if !b.deadline.Equal(at(wait)) {
			t.Fatalf("restart %s: deadline = %s, want unchanged %s", daemon, b.deadline.Sub(start), wait)
		}
	}
	// Real progress within the latest process still extends the bound.
	b.observe(at(18*time.Minute), startuphint.Hints{HasBudget: true, BudgetRemaining: time.Hour, Progress: "4", Daemon: "d"})
	if want := at(18*time.Minute + wait); !b.deadline.Equal(want) {
		t.Fatalf("progress after restart: deadline = %s, want %s", b.deadline.Sub(start), want.Sub(start))
	}
}

func TestProbeWaitsForAdvancingDaemonPastReadyWaitButNotForStuckOne(t *testing.T) {
	answer := func(progress func(n int32) string, readyAfter int32) (probePutter, *atomic.Int32, blobstore.Store) {
		local, _ := blobstore.NewDir(t.TempDir())
		var calls atomic.Int32
		return probePutterFunc(func(ctx context.Context, digest string, data []byte) error {
			n := calls.Add(1)
			time.Sleep(10 * time.Millisecond)
			if n > readyAfter {
				return local.Put(ctx, digest, data)
			}
			header := http.Header{}
			startuphint.Set(header, startuphint.Hints{Progress: progress(n)})
			return &dispatcher.BlobStatusError{StatusCode: http.StatusServiceUnavailable, Status: "503 Service Unavailable", Body: "daemon is starting", Header: header}
		}), &calls, local
	}
	opts := fastProbe
	opts.ReadyWait = 100 * time.Millisecond

	// Forty answers take well over ReadyWait, but each reports new progress.
	remote, calls, local := answer(func(n int32) string { return strconv.Itoa(int(n)) }, 40)
	if err := VerifyShared(context.Background(), local, remote, opts); err != nil {
		t.Fatalf("an advancing daemon was given up on after %d answers: %v", calls.Load(), err)
	}

	remote, calls, local = answer(func(int32) string { return "7" }, 1<<30)
	err := VerifyShared(context.Background(), local, remote, opts)
	if err == nil || !strings.Contains(err.Error(), "WORKER_DAEMON_NOT_READY") {
		t.Fatalf("a daemon reporting no progress must still hit the bound: %v", err)
	}
	if calls.Load() >= 40 {
		t.Fatalf("stuck daemon was probed %d times; the bound did not hold", calls.Load())
	}
}
