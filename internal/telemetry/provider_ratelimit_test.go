package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/providers"
)

func TestStageRateLimitObserverEmitsSafeTypedEvent(t *testing.T) {
	dir := PrepareStageTelemetryDir(t.TempDir())
	observer := NewStageRateLimitObserver(dir)
	fixed := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	observer.now = func() time.Time { return fixed }
	const credential = "rate-limit-secret-canary"

	observer.ObserveRateLimit(context.Background(), providers.RateLimitEvent{
		Provider:      providers.ProviderGitHub,
		Scope:         "api.github.com/repos/acme/app/issues",
		Delay:         2500 * time.Millisecond,
		Outcome:       providers.RateLimitOutcomeRetry,
		Endpoint:      "https://" + credential + "@api.github.com/issues?token=" + credential,
		RetryAfterRaw: credential,
	})

	data, err := os.ReadFile(filepath.Join(dir, eventsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), credential) {
		t.Fatalf("stage telemetry leaked credential-bearing provider metadata: %s", data)
	}
	events, dropped := readEmissionFile[stageEvent](filepath.Join(dir, eventsFile), validEvent)
	if dropped != 0 || len(events) != 1 {
		t.Fatalf("events = %#v, dropped = %d", events, dropped)
	}
	event := events[0]
	if event.Name != ProviderRateLimitEventName || !event.Time.Equal(fixed) {
		t.Fatalf("event identity = %#v", event)
	}
	if event.Attrs["provider"] != "github" ||
		event.Attrs["scope"] != "api.github.com/repos/acme/app/issues" ||
		event.Attrs["delay_ms"] != float64(2500) ||
		event.Attrs["outcome"] != "retry" {
		t.Fatalf("event attrs = %#v", event.Attrs)
	}
}
