package main

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestUpServesHealthAndStopsHTTPGracefully(t *testing.T) {
	root := initDeterministicDemo(t)
	address := freeLoopbackAddress(t)
	setAPIListenAddress(t, root, address)

	ctx, cancel := context.WithCancel(context.Background())
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
	if !health.Ready || health.APIVersion != "v1" || health.SchemaVersion != "v1" {
		t.Fatalf("health = %+v", health)
	}
	if health.Instance.Name != "example" || health.Instance.Environment != "dev" {
		t.Fatalf("instance = %+v", health.Instance)
	}
	if health.Freshness.ObservedAt.IsZero() || health.Freshness.DefinitionsLoadedAt.IsZero() {
		t.Fatalf("freshness = %+v", health.Freshness)
	}

	response, err = http.Get("http://" + address + httpapi.InstancePath)
	if err != nil {
		t.Fatal(err)
	}
	var inventory readservice.Instance
	if err := json.NewDecoder(response.Body).Decode(&inventory); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !inventory.Ready ||
		inventory.Counts.Gaggles == 0 || inventory.Counts.Workflows == 0 {
		t.Fatalf("inventory status/view = %d / %+v", response.StatusCode, inventory)
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
