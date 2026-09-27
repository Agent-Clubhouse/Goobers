package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readservice"
)

func setAPIListenAddress(t *testing.T, root, address string) {
	t.Helper()
	l := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.API.Listen = address
	if err := instance.WriteConfig(l.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
}

func freeLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// loopbackDaemon is a `goobers up` daemon started by startUpOnFreeLoopback.
// stderr may be read only after a value arrives on done.
type loopbackDaemon struct {
	root    string
	address string
	cancel  context.CancelFunc
	done    <-chan int
	stderr  *bytes.Buffer
}

// loopbackBindAttempts bounds startUpOnFreeLoopback's retries. Each collision
// needs an unrelated socket to land on the one port just released, so a second
// consecutive collision is already rare and a third means something else is
// wrong.
const loopbackBindAttempts = 3

// startUpOnFreeLoopback starts `goobers up --quiet <root>` with a daemon
// listener on an address from pickAddress and waits for startup. prepare
// builds a fresh instance root configured to listen on the given address.
//
// freeLoopbackAddress cannot reserve the port it returns: between closing its
// probe listener and the daemon binding, any socket on the machine may take
// the port, including an outbound connection, since connect() allocates local
// ports from the same ephemeral range as a ":0" listen. The daemon then exits
// before startup with "address already in use", a failure of the fixture, not
// of the code under test (#5475). The daemon binds the address itself, so the
// test cannot hand it a pre-bound listener; instead a startup that fails with
// that error is retried on a fresh root and a fresh address. Any other startup
// failure is fatal on the first attempt.
func startUpOnFreeLoopback(t *testing.T, pickAddress func(*testing.T) string, prepare func(address string) string) loopbackDaemon {
	t.Helper()
	for attempt := 1; ; attempt++ {
		address := pickAddress(t)
		root := prepare(address)
		ctx, cancel := context.WithCancel(context.Background())
		started := &daemonStartedWriter{started: make(chan struct{})}
		stderr := &bytes.Buffer{}
		done := make(chan int, 1)
		go func() {
			done <- runUpContext(ctx, []string{"--quiet", root}, started, stderr)
		}()
		select {
		case <-started.started:
			return loopbackDaemon{root: root, address: address, cancel: cancel, done: done, stderr: stderr}
		case code := <-done:
			cancel()
			if attempt < loopbackBindAttempts && strings.Contains(stderr.String(), "address already in use") {
				t.Logf("daemon startup attempt %d lost %s to another socket; retrying on a fresh address", attempt, address)
				continue
			}
			t.Fatalf("daemon exited before startup: code = %d, stderr = %q", code, stderr.String())
		case <-time.After(10 * time.Second):
			cancel()
			t.Fatal("timed out waiting for daemon startup")
		}
	}
}

// holdLoopbackAddress reserves a loopback address for the lifetime of the test
// and keeps it bound. Use it instead of freeLoopbackAddress when the assertion
// is that nothing else binds the address: releasing the port first leaves a
// window in which an unrelated process on the machine can claim it, which reads
// as a failure of the code under test.
func holdLoopbackAddress(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func TestUpServesHealthAndStopsHTTPGracefully(t *testing.T) {
	root := initDeterministicDemo(t)
	address := freeLoopbackAddress(t)
	setAPIListenAddress(t, root, address)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := &daemonStartedWriter{started: make(chan struct{})}
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runUpContext(ctx, []string{"--quiet", root}, started, &stderr)
	}()
	select {
	case <-started.started:
	case code := <-done:
		t.Fatalf("daemon exited before startup: code = %d, stderr = %q", code, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for daemon startup")
	}

	// A bounded client so the request fails fast rather than blocking forever if
	// it ever reaches a daemon that accepts the connection but never answers
	// (e.g. a co-located foreign daemon) — #798's "fail fast, don't hang" rule.
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get("http://" + address + httpapi.HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	var health readservice.Health
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if !health.Ready || !health.Healthy || health.APIVersion != "v1" || health.SchemaVersion != "v1" {
		t.Fatalf("health = %+v", health)
	}
	if health.Instance.Name != "example" || health.Instance.Environment != "dev" {
		t.Fatalf("instance = %+v", health.Instance)
	}
	identity, err := instance.NewLayout(root).ReadIdentity()
	if err != nil || identity == "" {
		t.Fatalf("real daemon startup did not persist its instance identity: %q, %v", identity, err)
	}
	if health.Freshness.ObservedAt.IsZero() || health.Freshness.DefinitionsLoadedAt.IsZero() {
		t.Fatalf("freshness = %+v", health.Freshness)
	}
	if health.Freshness.LastSchedulerTickAt == nil || health.Freshness.LastTickAgeMillis == nil {
		t.Fatalf("scheduler freshness = %+v", health.Freshness)
	}

	response, err = http.Get("http://" + address + httpapi.InstancePath)
	if err != nil {
		t.Fatal(err)
	}
	inventoryBody, bodyErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if bodyErr != nil {
		t.Fatal(bodyErr)
	}
	var inventory readservice.Instance
	if err := json.Unmarshal(inventoryBody, &inventory); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !inventory.Ready ||
		inventory.Counts.Gaggles == 0 || inventory.Counts.Workflows == 0 {
		cancel()
		select {
		case <-done:
			t.Fatalf("inventory status/view = %d / %+v; body=%s; daemon stderr=%s", response.StatusCode, inventory, inventoryBody, stderr.String())
		case <-time.After(10 * time.Second):
			t.Fatalf("inventory status/view = %d / %+v; body=%s; daemon did not stop", response.StatusCode, inventory, inventoryBody)
		}
	}

	response, err = http.Get("http://" + address + httpapi.GagglesPath)
	if err != nil {
		t.Fatal(err)
	}
	var gaggles readservice.GagglePage
	if err := json.NewDecoder(response.Body).Decode(&gaggles); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || len(gaggles.Items) == 0 {
		t.Fatalf("gaggles status/view = %d / %+v", response.StatusCode, gaggles)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not shut down")
	}

	closedClient := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	if _, err := closedClient.Get("http://" + address + httpapi.HealthPath); err == nil {
		t.Fatal("HTTP API still accepted requests after daemon shutdown")
	}
}

func TestUpSurfacesHTTPStartupFailure(t *testing.T) {
	root := initDeterministicDemo(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := occupied.Close(); err != nil {
			t.Errorf("close occupied listener: %v", err)
		}
	})
	setAPIListenAddress(t, root, occupied.Addr().String())

	var stdout, stderr bytes.Buffer
	code := runUpContext(context.Background(), []string{"--quiet", root}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "start HTTP API") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
