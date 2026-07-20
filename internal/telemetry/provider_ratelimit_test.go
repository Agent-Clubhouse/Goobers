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

func TestClientRateLimitObserverExportsStandaloneSafeEvent(t *testing.T) {
	exporter := NewMemoryExporter()
	client, err := New(context.Background(), Config{
		ServiceName:  "provider-rate-limit-test",
		SpanExporter: exporter,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	const credential = "rate-limit-secret-canary"

	client.ObserveRateLimit(context.Background(), providers.RateLimitEvent{
		Provider:      providers.ProviderADO,
		Scope:         "dev.azure.com/org/project/_apis/wit/wiql",
		Delay:         3 * time.Second,
		Outcome:       providers.RateLimitOutcomeRetry,
		Endpoint:      "https://" + credential + "@dev.azure.com?token=" + credential,
		RetryAfterRaw: credential,
	})

	spans := exporter.Spans()
	if len(spans) != 1 || spans[0].Name() != providerRateLimitSpanName {
		t.Fatalf("exported spans = %#v, want one %q span", spans, providerRateLimitSpanName)
	}
	events := spans[0].Events()
	if len(events) != 1 || events[0].Name != ProviderRateLimitEventName {
		t.Fatalf("span events = %#v, want one %q event", events, ProviderRateLimitEventName)
	}
	attrs := make(map[string]string, len(events[0].Attributes))
	for _, attr := range events[0].Attributes {
		attrs[string(attr.Key)] = attr.Value.Emit()
	}
	if attrs["provider"] != "ado" ||
		attrs["scope"] != "dev.azure.com/org/project/_apis/wit/wiql" ||
		attrs["delay_ms"] != "3000" ||
		attrs["outcome"] != "retry" {
		t.Fatalf("event attrs = %#v", attrs)
	}
	if strings.Contains(events[0].Name, credential) {
		t.Fatalf("rate-limit event leaked credential: %#v", events[0])
	}
}
