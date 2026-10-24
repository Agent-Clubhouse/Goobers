package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudauth "github.com/goobers/goobers/internal/cloud/podauth"
	"github.com/goobers/goobers/internal/httpapi"
	sharedauth "github.com/goobers/goobers/internal/podauth"
)

type controlTestLeadership struct{}

func (controlTestLeadership) Run(context.Context, func(context.Context, *lease.Handle) error) error {
	return lease.ErrHeld
}
func (controlTestLeadership) SelfFenced(*lease.Handle) bool { return true }

func TestControlBootRefusesSelfExecution(t *testing.T) {
	code, _, stderr := runForTest("control", "--self-execution", "allow")
	if code != 2 || !strings.Contains(stderr, "selfExecution: deny") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}

func TestControlReadinessRefusesMissingPlanesAndLease(t *testing.T) {
	host, err := control.New(control.Config{ValidateAssembly: func() error { return nil }, SelfExecution: "deny", Leadership: controlTestLeadership{}, SchemaCompatible: func(context.Context) error { return nil }, BuildEpoch: func(context.Context, *lease.Handle) (control.EpochServices, error) {
		return control.EpochServices{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	db := databaseServices{StartStatusPage: func(context.Context, string, int) ([]acceptance.StartStatus, error) { return nil, nil }}
	handler, err := controlHandler(db, controlPlanes{}, &cloudauth.Auth{}, host, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness=%d %s", response.Code, response.Body.String())
	}
}

type activeControlTestLeadership struct{}

func (activeControlTestLeadership) Run(ctx context.Context, work func(context.Context, *lease.Handle) error) error {
	return work(ctx, &lease.Handle{})
}
func (activeControlTestLeadership) SelfFenced(*lease.Handle) bool { return false }

func TestControlLifetimeReportsStopTimeoutOnSIGTERM(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, unblock := make(chan struct{}), make(chan struct{})
	defer close(unblock)
	ok := func(context.Context) error { return nil }
	host, err := control.New(control.Config{ValidateAssembly: func() error { return nil }, SelfExecution: "deny", Leadership: activeControlTestLeadership{}, SchemaCompatible: ok, StopTimeout: 10 * time.Millisecond, BuildEpoch: func(context.Context, *lease.Handle) (control.EpochServices, error) {
		return control.EpochServices{RebuildBeforeReap: ok, ReconcileUncertain: ok, ReattachAwaiters: ok, Stop: ok, L2: ok, L3: ok, L1: func(context.Context) error { close(entered); <-unblock; return nil }}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "keys.json")
	raw, err := json.Marshal(map[string]sharedauth.SigningKey{"current": {ID: "test", Key: bytes.Repeat([]byte("k"), 32)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	key, err := sharedauth.LoadKeyRingFile(path, "")
	if err != nil {
		t.Fatal(err)
	}
	auth := &cloudauth.Auth{PodKeys: key, GrantKeys: key}
	logger := log.New(io.Discard, "", 0)
	server, err := httpapi.NewServer("127.0.0.1:0", http.NotFoundHandler(), logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	// A background service (the projector) is cancelled and awaited with the rest.
	var backgroundStopped atomic.Bool
	background := func(ctx context.Context) error {
		<-ctx.Done()
		backgroundStopped.Store(true)
		return ctx.Err()
	}
	go func() { done <- runControlLifetime(ctx, cancel, host, auth, server, func(error) {}, background) }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, lease.ErrWorkStillRunning) {
		t.Fatalf("SIGTERM hid hung epoch: %v", err)
	}
	if !backgroundStopped.Load() {
		t.Fatal("shutdown returned before the background service stopped")
	}
}

func TestControlAssemblyValidationRefusesMissingPlanes(t *testing.T) {
	if err := (controlPlanes{}).validate(); err == nil {
		t.Fatal("empty assembly was allowed to contend for leadership")
	}
}
