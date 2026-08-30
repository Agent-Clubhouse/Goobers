package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuidedInitReturnsMigrationWithoutReadingInputOrWritingFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instance")
	input := &failingGuidedInput{}
	var stdout, stderr bytes.Buffer

	code := runInitWithInput([]string{"--guided", root}, input, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("guided init code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("guided init stdout = %q, want empty", stdout.String())
	}
	for _, want := range []string{
		"`goobers init --guided` has been removed",
		"goobers getting-started",
		"Goobers Getting Started skill",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("guided init stderr = %q, missing %q", stderr.String(), want)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("guided init wrote target path: %v", err)
	}
}

func TestGuidedInitMigrationWinsOverOtherModes(t *testing.T) {
	var stderr bytes.Buffer
	code := runInitWithInput(
		[]string{"--guided", "--demo", filepath.Join(t.TempDir(), "instance")},
		strings.NewReader("must not be read"),
		io.Discard,
		&stderr,
	)
	if code != 2 || !strings.Contains(stderr.String(), "has been removed") {
		t.Fatalf("guided migration = code %d stderr %q", code, stderr.String())
	}
}

type failingGuidedInput struct{}

func (*failingGuidedInput) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}
