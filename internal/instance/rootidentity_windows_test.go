//go:build windows

package instance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestEnsureRootIdentityWaitsForContendedIdentityRead(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(NewLayout(root).ConfigFile(), []byte("existing config"), 0o600); err != nil {
		t.Fatal(err)
	}
	const want = "0123456789abcdef0123456789abcdef"
	path := filepath.Join(root, RootIdentityFileName)
	if err := os.WriteFile(path, []byte(want+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lock, err := acquireRootIdentityLock(context.Background(), path+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		path16,
		windows.GENERIC_READ,
		0,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	type result struct {
		id  string
		err error
	}
	resultC := make(chan result, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		id, err := EnsureRootIdentity(ctx, root)
		resultC <- result{id: id, err: err}
	}()

	select {
	case got := <-resultC:
		t.Fatalf("EnsureRootIdentity returned while the adoption lock was held: id=%q err=%v", got.id, got.err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	handle = 0
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	got := <-resultC
	if got.err != nil || got.id != want {
		t.Fatalf("EnsureRootIdentity() = %q, %v; want %q, nil", got.id, got.err, want)
	}
}
