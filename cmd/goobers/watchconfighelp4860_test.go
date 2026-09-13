package main

import (
	"strings"
	"testing"
)

func TestUpHelpExplainsWatchConfigBoundary(t *testing.T) {
	for _, want := range []string{
		"Direct edits to the materialized",
		"config directory are watched by default",
		"instance.yaml is loaded only at daemon startup",
		"retention:",
		"telemetry.retention:",
		"require a daemon restart",
	} {
		if !strings.Contains(upHelp, want) {
			t.Errorf("up help does not contain %q", want)
		}
	}
}

func TestWatchConfigCompletionIsDefaultOnAndNotExperimental(t *testing.T) {
	var description string
	for _, spec := range completionFlagSpecs["up"] {
		if spec.name == "watch-config" {
			description = spec.desc
			break
		}
	}
	if description == "" || !strings.Contains(description, "default on") ||
		!strings.Contains(description, "instance.yaml requires restart") ||
		strings.Contains(strings.ToLower(description), "experimental") {
		t.Fatalf("watch-config completion description = %q", description)
	}
}
