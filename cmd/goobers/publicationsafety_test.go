package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/goobers/goobers/internal/diagnostictmp"
)

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
