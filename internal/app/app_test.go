package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestRun_VersionFlagExitsZero(t *testing.T) {
	var buf bytes.Buffer
	called := false
	code := run("scheduler", []string{"--version"}, &buf, func(context.Context, *slog.Logger) error {
		called = true
		return nil
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if called {
		t.Fatal("RunFunc should not be invoked when --version is set")
	}
	if !strings.Contains(buf.String(), "scheduler") {
		t.Fatalf("version output missing binary name: %q", buf.String())
	}
}

func TestRun_InvalidFlagExitsTwo(t *testing.T) {
	var buf bytes.Buffer
	code := run("operator", []string{"--nope"}, &buf, func(context.Context, *slog.Logger) error {
		return nil
	})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRun_SuccessReturnsZero(t *testing.T) {
	var buf bytes.Buffer
	var gotCtx context.Context
	code := run("operator", nil, &buf, func(ctx context.Context, log *slog.Logger) error {
		gotCtx = ctx
		log.Info("hello")
		return nil
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if gotCtx == nil {
		t.Fatal("RunFunc received nil context")
	}
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("logger output missing message: %q", buf.String())
	}
}

func TestRun_ErrorReturnsOne(t *testing.T) {
	var buf bytes.Buffer
	code := run("runtime", nil, &buf, func(context.Context, *slog.Logger) error {
		return errors.New("boom")
	})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "boom") {
		t.Fatalf("error not logged: %q", buf.String())
	}
}

func TestRunWithScrubberRedactsLogOutput(t *testing.T) {
	const secret = "opaque-runtime-credential"
	var buf bytes.Buffer
	code := runWithScrubber("runtime", nil, &buf, replaceScrubber{
		old: secret,
		new: "[REDACTED]",
	}, func(_ context.Context, log *slog.Logger) error {
		log.Error("provider request failed", "authorization", secret)
		return errors.New("worker failed with " + secret)
	})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("secret leaked into process output: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "[REDACTED]") {
		t.Fatalf("redaction marker missing from process output: %q", buf.String())
	}
}

type replaceScrubber struct {
	old string
	new string
}

func (s replaceScrubber) Scrub(in []byte) []byte {
	return bytes.ReplaceAll(in, []byte(s.old), []byte(s.new))
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"bogus":   slog.LevelInfo,
		"":        slog.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewLogger_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf, "debug", "text")
	log.Debug("line", "k", "v")
	out := buf.String()
	if !strings.Contains(out, "k=v") {
		t.Fatalf("text handler output unexpected: %q", out)
	}
}
