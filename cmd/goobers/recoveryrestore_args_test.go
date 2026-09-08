package main

import (
	"io"
	"testing"
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
