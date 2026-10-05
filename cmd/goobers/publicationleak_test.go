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

	"github.com/goobers/goobers/internal/decisiongate"
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
