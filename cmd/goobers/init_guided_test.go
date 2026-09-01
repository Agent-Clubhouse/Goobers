package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuidedInitRejectsOtherInitModes(t *testing.T) {
	var stderr bytes.Buffer
	code := runInitWithInput(
		[]string{"--guided", "--demo", filepath.Join(t.TempDir(), "instance")},
		strings.NewReader("must not be read"),
		io.Discard,
		&stderr,
	)
	if code != 2 {
		t.Fatalf("guided init code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--guided, --demo, and --template cannot be combined") {
		t.Fatalf("guided init stderr = %q", stderr.String())
	}
}

func TestGuidedInitHelpDescribesBrowserSetup(t *testing.T) {
	var stderr bytes.Buffer
	if code := runInitWithInput([]string{"--help"}, strings.NewReader(""), io.Discard, &stderr); code != 2 {
		t.Fatalf("help code = %d", code)
	}
	for _, want := range []string{
		"--guided",
		"--instance-path",
		"browser-based setup",
		"does not run a workflow",
		"https://github.com/settings/personal-access-tokens/new",
		"Resource owner",
		"Only select repositories",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("init help = %q, missing %q", stderr.String(), want)
		}
	}
}

func TestGuidedInitRejectsPath(t *testing.T) {
	var stderr bytes.Buffer
	code := runInitWithInput(
		[]string{"--guided", filepath.Join(t.TempDir(), "instance")},
		strings.NewReader(""),
		io.Discard,
		&stderr,
	)
	if code != 2 {
		t.Fatalf("guided init path code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--guided does not accept a path") {
		t.Fatalf("guided init path stderr = %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--instance-path <dir>") {
		t.Fatalf("guided init path stderr = %q, missing instance-path guidance", stderr.String())
	}
}

func TestGuidedInitInstancePathRequiresGuidedMode(t *testing.T) {
	var stderr bytes.Buffer
	code := runInitWithInput(
		[]string{"--instance-path", filepath.Join(t.TempDir(), "instance")},
		strings.NewReader(""),
		io.Discard,
		&stderr,
	)
	if code != 2 || !strings.Contains(stderr.String(), "--instance-path requires --guided") {
		t.Fatalf("instance path code=%d stderr=%q, want guided-only usage error", code, stderr.String())
	}
}

func TestGuidedInitUnsafeInstancePathShowsOverrideInvocation(t *testing.T) {
	target := filepath.Join(t.TempDir(), "instance")
	t.Setenv("RUNNER_ENVIRONMENT", "github-hosted")
	var stderr bytes.Buffer
	code := runInitWithInput(
		[]string{"--guided", "--instance-path", target, "--no-open"},
		strings.NewReader(""),
		io.Discard,
		&stderr,
	)
	want := `goobers init --guided --instance-path "` + target + `" --allow-ephemeral`
	if code != 2 || !strings.Contains(stderr.String(), want) {
		t.Fatalf("unsafe guided code=%d stderr=%q, want %q", code, stderr.String(), want)
	}
}
