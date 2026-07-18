package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFsyncDisabledHonorsEnv(t *testing.T) {
	t.Setenv(envDisableFsync, "1")
	if !fsyncDisabled() {
		t.Fatalf("fsyncDisabled() = false with %s=1, want true", envDisableFsync)
	}
	t.Setenv(envDisableFsync, "0")
	if fsyncDisabled() {
		t.Fatalf("fsyncDisabled() = true with %s=0, want false", envDisableFsync)
	}
	_ = os.Unsetenv(envDisableFsync)
	if fsyncDisabled() {
		t.Fatalf("fsyncDisabled() = true with %s unset, want false (production default)", envDisableFsync)
	}
}

func TestDurableWritesStillPersistWithFsyncDisabled(t *testing.T) {
	t.Setenv(envDisableFsync, "1")

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	want := []byte("durable-bytes\n")
	if err := writeFileSynced(path, want, 0o644); err != nil {
		t.Fatalf("writeFileSynced with fsync disabled: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("content = %q, want %q — disabling fsync must not change what is written", got, want)
	}
	// fsyncDir must be a no-op that still succeeds when fsync is disabled.
	if err := fsyncDir(dir); err != nil {
		t.Fatalf("fsyncDir with fsync disabled = %v, want nil", err)
	}
}
