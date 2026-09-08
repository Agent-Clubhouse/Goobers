package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/claimsclient"
)

func TestRecoveryRestoreRefusesMixedOrIncompleteSelection(t *testing.T) {
	for _, selection := range [][]string{
		nil,
		{"--issue", "7"},
		{"--repository-key", "github|||team|repo|"},
		{"--record", "record.json", "--issue", "7"},
		{"--record", "record.json", "--repository-key", "github|||team|repo|"},
		{"--record", "record.json", "--issue", "7", "--repository-key", "github|||team|repo|"},
	} {
		args := append([]string{"--repository", "checkout", "--branch", "restored"}, selection...)
		if code := runRecoveryRestore(args, io.Discard, io.Discard); code != 2 {
			t.Fatalf("invalid selector %v returned %d, want usage error", selection, code)
		}
	}
}

func TestRecoveryRestorePartialPlaneDoesNotAdoptLocalRoot(t *testing.T) {
	for _, selector := range [][]string{
		{"--issue", "7", "--repository-key", "github|||team|repo|"},
		{"--record", "record.json"},
	} {
		t.Run(selector[0], func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "must-not-create")
			t.Setenv(claimsclient.EnvEndpoint, "http://daemon.invalid")
			t.Setenv(claimsclient.EnvToken, "")
			t.Setenv(claimsclient.EnvRunID, "receiving-run")
			args := append([]string{"--repository", "checkout", "--branch", "restored"}, selector...)
			args = append(args, root)
			if code := runRecoveryRestore(args, io.Discard, io.Discard); code != 1 {
				t.Fatalf("partial plane returned %d", code)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("plane invocation touched the local instance root: %v", err)
			}
		})
	}
}
