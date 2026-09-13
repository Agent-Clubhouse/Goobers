package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

type heldInitialCountReader struct {
	readmodel.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	err     error
}

func (r *heldInitialCountReader) ActiveRunCounts(ctx context.Context) ([]readmodel.WorkflowCount, error) {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
		if r.err != nil {
			return nil, r.err
		}
		return r.Reader.ActiveRunCounts(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestDaemonReadinessWaitsForInitialActiveCounts(t *testing.T) {
	for _, mode := range []string{"success", "sample-error", "shutdown-before-readiness"} {
		t.Run(mode, func(t *testing.T) { testDaemonInitialCounts(t, mode) })
	}
}

func testDaemonInitialCounts(t *testing.T, mode string) {
	t.Helper()
	root := initDeterministicDemo(t)
	address := freeLoopbackAddress(t)
	setAPIListenAddress(t, root, address)
	held := &heldInitialCountReader{entered: make(chan struct{}), release: make(chan struct{})}
	if mode == "sample-error" {
		held.err = errors.New("injected initial active-count query failure")
	}
	original := newDaemonReadService
	newDaemonReadService = func(s readservice.LocalSources, ready func() bool) (*readservice.Local, error) {
		held.Reader = s.ReadModel
		s.ReadModel = held
		return readservice.NewLocal(s, ready)
	}
	t.Cleanup(func() { newDaemonReadService = original })
	ctx, cancel := context.WithCancel(context.Background())
	stdout := newStartupHoldWriter("startup phase=active-counts status=start")
	stderr := newStartupHoldWriter("never-hold-stderr")
	done := make(chan int, 1)
	go func() { done <- runUpContext(ctx, []string{"--quiet", root}, stdout, stderr) }()
	joined := false
	defer func() {
		cancel()
		stdout.Release()
		if !joined {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("daemon shutdown timed out")
			}
		}
	}()
	select {
	case <-held.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("initial sample not entered")
	}
	select {
	case <-stdout.held:
	case code := <-done:
		joined = true
		t.Fatalf("startup exited %d: %s", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("startup did not reach sample readiness gate")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	var health readservice.Health
	getStartupJSON(t, client, address, httpapi.HealthPath, http.StatusOK, &health)
	if health.Ready || !health.Healthy {
		t.Fatalf("held first query health=%+v", health)
	}
	var readiness httpapi.ReadinessStatus
	// #4252: Ready (plane-ready) is already true here — the full versioned
	// handler, planes included, has been live since well before this point —
	// so the probe answers 200. SchedulerReady is the gate this test is
	// actually pinning: it must stay false until the initial active-count
	// sample completes, same as it always has.
	getStartupJSON(t, client, address, httpapi.ReadinessPath, http.StatusOK, &readiness)
	if !readiness.Ready {
		t.Fatal("probe did not advertise plane-ready even though the full handler is live")
	}
	if readiness.SchedulerReady {
		t.Fatal("probe advertised schedulerReady without initial active counts")
	}
	// All prior startup subsystems have completed. Only the first observation is
	// held, so this pins its dependency rather than a generic slow startup.
	for _, check := range []string{"configLoaded", "stateOpen", "resumeComplete", "sweepsStarted"} {
		if !readiness.Checks[check] {
			t.Fatalf("prior startup subsystem %s incomplete", check)
		}
	}
	stdout.Release()
	// Startup can now reach the real waiter; keep the query held across an
	// observable scheduling interval so bypassing the waiter advertises ready.
	select {
	case <-stdout.started:
		t.Fatal("daemon advertised startup while the first query was still held")
	case code := <-done:
		joined = true
		t.Fatalf("startup did not wait for its first query: code=%d stderr=%s", code, stderr.String())
	case <-time.After(100 * time.Millisecond):
	}
	getStartupJSON(t, client, address, httpapi.ReadinessPath, http.StatusOK, &readiness)
	if readiness.SchedulerReady {
		t.Fatal("schedulerReady flipped true before the active-count sample completed")
	}
	if mode == "shutdown-before-readiness" {
		cancel()
	} else {
		close(held.release)
	}
	if mode == "success" {
		select {
		case <-stdout.started:
		case <-time.After(10 * time.Second):
			t.Fatal("daemon did not become ready after successful sample")
		}
		getStartupJSON(t, client, address, httpapi.HealthPath, http.StatusOK, &health)
		getStartupJSON(t, client, address, httpapi.ReadinessPath, http.StatusOK, &readiness)
		if !health.Ready || !readiness.Ready || !readiness.SchedulerReady {
			t.Fatal("successful sample did not open shared readiness")
		}
		var inventory readservice.Instance
		getStartupJSON(t, client, address, httpapi.InstancePath, http.StatusOK, &inventory)
		if !inventory.Ready || inventory.Counts.Gaggles == 0 || inventory.Counts.Workflows == 0 {
			t.Fatalf("ready inventory=%+v", inventory)
		}
		cancel()
	}
	select {
	case code := <-done:
		joined = true
		if mode == "success" {
			if code != 0 {
				t.Fatalf("shutdown=%d: %s", code, stderr.String())
			}
			return
		}
		if mode == "shutdown-before-readiness" {
			if code != 0 {
				t.Fatalf("pre-readiness shutdown code=%d, want documented clean-shutdown code 0; stderr=%s", code, stderr.String())
			}
			if strings.Contains(stderr.String(), "error: initialize active-run counts:") {
				t.Fatalf("clean pre-readiness shutdown reported a startup failure: %s", stderr.String())
			}
		} else if code == 0 || !strings.Contains(stderr.String(), "error: initialize active-run counts:") {
			t.Fatalf("startup failure code=%d stderr=%s", code, stderr.String())
		}
		select {
		case <-stdout.started:
			t.Fatal("failed initial sample advertised daemon started")
		default:
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not settle after sample/cancellation")
	}
}

func getStartupJSON(t *testing.T, client *http.Client, address, path string, status int, out any) {
	t.Helper()
	resp, err := client.Get("http://" + address + path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status {
		t.Fatalf("%s status=%d want=%d body=%s", path, resp.StatusCode, status, b)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s decode: %v; body=%s", path, err, b)
	}
}
