package query

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestMaterializeArtifactGrantedProducesJSON(t *testing.T) {
	type stats struct {
		TotalRuns int `json:"totalRuns"`
	}
	b, err := MaterializeArtifact([]string{"repo:push", CapabilityRead}, stats{TotalRuns: 3})
	if err != nil {
		t.Fatalf("MaterializeArtifact: %v", err)
	}
	var got stats
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode artifact bytes: %v", err)
	}
	if got.TotalRuns != 3 {
		t.Fatalf("got.TotalRuns = %d, want 3", got.TotalRuns)
	}
}

func TestMaterializeArtifactWithoutGrantFailsClosed(t *testing.T) {
	b, err := MaterializeArtifact([]string{"repo:push"}, map[string]int{"x": 1})
	if !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("expected ErrCapabilityDenied, got %v", err)
	}
	if b != nil {
		t.Fatalf("expected no bytes produced on denial, got %q", b)
	}
}

func TestMaterializeArtifactEmptyCapabilitiesFailsClosed(t *testing.T) {
	if _, err := MaterializeArtifact(nil, map[string]int{"x": 1}); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("expected ErrCapabilityDenied for nil capabilities, got %v", err)
	}
}

func TestHasCapability(t *testing.T) {
	caps := []string{"github:issues:write", CapabilityRead}
	if !HasCapability(caps, CapabilityRead) {
		t.Fatal("expected telemetry:read to be found")
	}
	if HasCapability(caps, "repo:push") {
		t.Fatal("expected repo:push to be absent")
	}
	if HasCapability(nil, CapabilityRead) {
		t.Fatal("expected nil capabilities to grant nothing")
	}
}

func TestNotConfiguredSourceFailsClearly(t *testing.T) {
	var src ProjectTelemetrySource = NotConfiguredSource{}
	_, err := src.Query(context.Background(), ProjectQueryRequest{Query: "select 1"})
	if !errors.Is(err, ErrProjectTelemetryNotConfigured) {
		t.Fatalf("expected ErrProjectTelemetryNotConfigured, got %v", err)
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("error message should say not configured: %q", err.Error())
	}
}
