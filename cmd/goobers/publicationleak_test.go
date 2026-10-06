package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

func TestPublicationLeakScreenRequiresExplicitShadowOptIn(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{
				decisiongate.PublicationLeakQuestion: map[string]any{"type": "noul", "noul": 0.95},
			},
		})
	}))
	t.Cleanup(server.Close)
	t.Setenv("LEAK_SCREEN_URL", server.URL)
	t.Setenv("LEAK_SCREEN_KEY", "test-key")
	t.Setenv("LEAK_SCREEN_MODEL", "test-model")

	settings := &decisiongate.Settings{
		BaseURLEnv: "LEAK_SCREEN_URL", KeyEnv: "LEAK_SCREEN_KEY", ModelEnv: "LEAK_SCREEN_MODEL",
		Fallback: decisiongate.FallbackAgent,
	}
	for _, mode := range []decisiongate.Mode{"", decisiongate.ModeOff, decisiongate.ModeEnforce} {
		settings.Mode = mode
		screen := newPublicationLeakScreen(&instance.Config{DecisionGate: settings}, nil)
		if screen != nil {
			screen(context.Background(), "issue", "title", "body")
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("off or enforce mode sent %d requests", calls.Load())
	}

	settings.Mode = decisiongate.ModeShadow
	if screen := newPublicationLeakScreen(&instance.Config{DecisionGate: settings}, nil); screen != nil {
		screen(context.Background(), "issue", "title", "body")
	}
	if calls.Load() != 0 {
		t.Fatalf("shadow mode without publication opt-in sent %d requests", calls.Load())
	}

	var logs bytes.Buffer
	settings.PublicationLeakScreen = true
	screen := newPublicationLeakScreen(
		&instance.Config{DecisionGate: settings},
		slog.New(slog.NewTextHandler(&logs, nil)),
	)
	if screen == nil {
		t.Fatal("shadow opt-in did not create a screen")
	}
	screen(context.Background(), "issue", "title", "body")
	if calls.Load() != 1 {
		t.Fatalf("explicit publication opt-in sent %d requests, want 1", calls.Load())
	}
	if got := logs.String(); !strings.Contains(got, "decisiongate.publication-shadow") ||
		strings.Contains(got, "title") || strings.Contains(got, "body") {
		t.Fatalf("unexpected advisory log: %q", got)
	}
}

func TestOptedInPodPublicationScreensBeforePublishing(t *testing.T) {
	var sequence atomic.Int32
	var screenedAt atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		screenedAt.Store(sequence.Add(1))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{
				decisiongate.PublicationLeakQuestion: map[string]any{"type": "noul", "noul": 0.95},
			},
		})
	}))
	t.Cleanup(server.Close)

	settings := &decisiongate.Settings{
		Mode: decisiongate.ModeShadow, PublicationLeakScreen: true,
		BaseURLEnv: "LEAK_SCREEN_URL", KeyEnv: "LEAK_SCREEN_KEY", ModelEnv: "LEAK_SCREEN_MODEL",
		Fallback: decisiongate.FallbackAgent,
	}
	t.Setenv("LEAK_SCREEN_URL", server.URL)
	t.Setenv("LEAK_SCREEN_KEY", "pod-test-key")
	t.Setenv("LEAK_SCREEN_MODEL", "pod-test-model")
	service := daemonCredentialService{config: &instance.Config{DecisionGate: settings}}
	delivery := service.publicationLeakScreenDelivery(
		httpapi.CredentialResolveRequest{Capabilities: []string{string(capability.ProviderPRWrite)}},
		stageResolution{profile: stageProfile{deterministic: true}},
	)
	if delivery == nil {
		t.Fatal("daemon did not deliver the opted-in publication screen")
	}
	if got := service.publicationLeakScreenDelivery(
		httpapi.CredentialResolveRequest{Capabilities: []string{string(capability.GitHubIssuesRead)}},
		stageResolution{profile: stageProfile{deterministic: true}},
	); got != nil {
		t.Fatal("daemon delivered model access to a stage without publication authority")
	}
	creds := podStageCredentials{publicationLeakScreen: &dispatcher.PublicationLeakScreen{
		Settings: delivery.Settings, BaseURL: delivery.BaseURL, APIKey: delivery.APIKey, Model: delivery.Model,
	}}
	if scrubbed := creds.withGrant(nil); len(scrubbed) != 1 || scrubbed[0].Value != "pod-test-key" {
		t.Fatalf("publication model key was not registered for pod output scrubbing: %+v", scrubbed)
	}
	t.Setenv(dispatcher.EnvPodAttempt, "1")
	for _, entry := range creds.env() {
		name, value, _ := strings.Cut(entry, "=")
		if name == dispatcher.EnvPublicationLeakScreen {
			t.Setenv(name, value)
		}
	}

	screen := publicationLeakScreenForRoot(t.TempDir(), nil)
	if screen == nil {
		t.Fatal("pod invocation did not install the delivered publication screen")
	}
	screen(context.Background(), "pull-request", "title", "body")
	publishedAt := sequence.Add(1)
	if screenedAt.Load() == 0 || screenedAt.Load() >= publishedAt {
		t.Fatalf("screened at %d, published at %d; want screening first", screenedAt.Load(), publishedAt)
	}
}
