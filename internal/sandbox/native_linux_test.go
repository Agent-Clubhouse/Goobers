//go:build linux

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestNewRejectsUnusableBubblewrap(t *testing.T) {
	bin := t.TempDir()
	path := filepath.Join(bin, "bwrap")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 42\n"), 0o700); err != nil {
		t.Fatalf("write fake bubblewrap: %v", err)
	}
	t.Setenv("PATH", bin)

	if _, err := New(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("New error = %v, want ErrUnavailable", err)
	}
}

func TestBubblewrapReadMasksFollowAllWritableBinds(t *testing.T) {
	workspace := t.TempDir()
	private := t.TempDir()
	key := filepath.Join(private, "controller.key")
	if err := os.WriteFile(key, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(private, "wrapping")
	if err := os.Mkdir(keys, 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/true")
	if err := (nativeSandbox{bubblewrapPath: "/fixture/bwrap"}).Wrap(command, Policy{Workspace: workspace, ReadDeniedPaths: []string{key, keys}}); err != nil {
		t.Fatal(err)
	}
	mask := slices.Index(command.Args, "/dev/null")
	write := slices.Index(command.Args, "--bind")
	if mask <= write || mask+1 >= len(command.Args) || command.Args[mask+1] != key {
		t.Fatalf("file mask ordering: %v", command.Args)
	}
	for _, flag := range []string{"--tmpfs", "--remount-ro"} {
		index := slices.Index(command.Args, flag)
		if index <= write || index+1 >= len(command.Args) || command.Args[index+1] != keys {
			t.Fatalf("directory mask %s absent", flag)
		}
	}
}
