package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/diagnostictmp"
)

func TestPublicationSafetyPreservesContextError(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[expired], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			want := context.Canceled
			if expired {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			}
			cancel()
			// No Git process is needed once the caller's budget is exhausted.
			t.Setenv("PATH", t.TempDir())
			if tip, err := checkPublicationSafety(ctx, t.TempDir(), "main", "HEAD"); tip != "" || !errors.Is(err, want) {
				t.Fatalf("publication tip=%q error=%v, want refusal preserving %v", tip, err, want)
			}
		})
	}
}

func TestPublicationReadLimitFailsClosed(t *testing.T) {
	b := &publicationBuffer{}
	if _, err := b.Write(make([]byte, publicationReadLimit+1)); err == nil {
		t.Fatal("oversized inspection accepted")
	}
	if b.Len() != 0 {
		t.Fatal("oversized inspection was partially retained")
	}
}

func TestSweepOrphanedDiagnosticsPreservesLiveCaptures(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	if runtime.GOOS == "windows" {
		t.Setenv("TMP", root)
		t.Setenv("TEMP", root)
	}
	live, err := diagnostictmp.Establish("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := live.Reclaim(); err != nil {
			t.Error(err)
		}
	})
	orphan := filepath.Join(filepath.Dir(live.Dir()), "scope-abandoned")
	if err := os.Mkdir(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	sweepOrphanedDiagnostics(nil)
	if _, err := os.Stat(live.Dir()); err != nil {
		t.Fatalf("live capture removed: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("abandoned capture retained: %v", err)
	}
}
