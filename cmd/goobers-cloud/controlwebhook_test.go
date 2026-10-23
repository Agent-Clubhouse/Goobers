package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/control"
	cloudpg "github.com/goobers/goobers/internal/cloud/pg"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/webhook"
)

type plainConfiguration struct{ control.Configuration }

func webhookDatabases() controlDatabases {
	return controlDatabases{API: databaseServices{Pools: &cloudpg.Pools{Transaction: &pgxpool.Pool{}}}, InstanceID: "i"}
}

func TestMountWebhookIsOptIn(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	got, stop, err := mountWebhook(context.Background(), controlOptions{}, webhookDatabases(), nil, api, log.New(&bytes.Buffer{}, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	rec := httptest.NewRecorder()
	got.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, webhook.Path, nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("without a secret the path reaches the API handler, got %d", rec.Code)
	}
}

func TestMountWebhookRequiresRecipientsAndFailsClosed(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := controlOptions{webhookSecretFile: secret}
	api := http.NotFoundHandler()
	logger := log.New(&bytes.Buffer{}, "", 0)
	if _, _, err := mountWebhook(context.Background(), options, webhookDatabases(), plainConfiguration{}, api, logger); err == nil {
		t.Fatal("a configuration that cannot resolve recipients was accepted")
	}
	if _, _, err := mountWebhook(context.Background(), options, webhookDatabases(), nil, api, logger); err == nil {
		t.Fatal("a missing configuration was accepted")
	}
}

type noActivator struct{}

func (noActivator) Activate(context.Context, cloudconfig.Candidate) error { return nil }

func TestMountedWebhookAuthenticatesBeforeAnything(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := cloudconfig.NewConfiguration(
		func(*instance.Config, *instance.ConfigSet) cloudconfig.Assessment { return nil }, noActivator{},
		func(context.Context) (cloudconfig.Candidate, error) { return cloudconfig.Candidate{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	var logs bytes.Buffer
	got, stop, err := mountWebhook(context.Background(), controlOptions{webhookSecretFile: secret}, webhookDatabases(), configuration, api, log.New(&logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	// An unsigned delivery is refused before intake touches the (unusable) pool.
	rec := httptest.NewRecorder()
	got.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, webhook.Path, strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned delivery status %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	got.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/anything", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("other paths must reach the API handler, got %d", rec.Code)
	}
}

func TestMountedWebhookKeepsOffLoopbackAuthenticationEvidence(t *testing.T) {
	logger := log.New(&bytes.Buffer{}, "", 0)
	api, err := httpapi.NewControlStatusHandler(httpapi.DenyAllAuthenticator{}, func(context.Context) (any, error) { return nil, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	tlsOption := func(s *http.Server) error {
		s.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		return nil
	}
	if _, err := httpapi.NewServer("0.0.0.0:0", api, logger, tlsOption); err != nil {
		t.Fatalf("premise: authenticated handler rejected off loopback: %v", err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := cloudconfig.NewConfiguration(
		func(*instance.Config, *instance.ConfigSet) cloudconfig.Assessment { return nil }, noActivator{},
		func(context.Context) (cloudconfig.Candidate, error) { return cloudconfig.Candidate{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	mounted, stop, err := mountWebhook(context.Background(), controlOptions{webhookSecretFile: secret}, webhookDatabases(), configuration, api, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if _, err := httpapi.NewServer("0.0.0.0:0", mounted, logger, tlsOption); err != nil {
		t.Fatalf("mounting the webhook lost the authenticated-transport evidence: %v", err)
	}
}
