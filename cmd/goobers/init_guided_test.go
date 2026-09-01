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
}
