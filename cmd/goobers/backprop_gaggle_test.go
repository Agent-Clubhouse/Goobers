package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestTelemetryGaggleRequiresGaggleAndToleratesEmptyInstance(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runTelemetryGaggle([]string{t.TempDir()}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit = %d without --gaggle, want 2 (stderr %q)", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runTelemetryGaggle([]string{"--gaggle=web", t.TempDir()}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "gaggle web: 0 active Backprop run(s)") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}
