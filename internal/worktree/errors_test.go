package worktree

import (
	"errors"
	"fmt"
	"net"
	"testing"
)

// TestClassifyProvisionError is the taxonomy acceptance: the three regimes
// that shared one opaque bucket in the 2026-08-08 audit — an unauthorized
// clone, a credential helper that would not exec, and a DNS outage — must
// classify to two DIFFERENT owners, which git's uniform exit 128 alone cannot
// express. Retryability is deliberately not the axis: a remote 5xx is network
// and retryable, a 403 is git and permanent, a promisor backfill is retryable
// yet not provably network.
func TestClassifyProvisionError(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		message  string
		want     FailureTier
	}{
		{
			name:     "dns outage",
			exitCode: 128,
			message:  "fatal: unable to access 'https://example.test/repo.git/': Could not resolve host: example.test",
			want:     TierNetwork,
		},
		{
			name:     "connection refused",
			exitCode: 128,
			message:  "fatal: unable to connect to example.test:\nexample.test[0: 192.0.2.1]: errno=Connection refused",
			want:     TierNetwork,
		},
		{
			name:     "remote 5xx",
			exitCode: 128,
			message:  "fatal: unable to access 'https://example.test/repo.git/': The requested URL returned error: 503",
			want:     TierNetwork,
		},
		{
			name:     "clone not authorized",
			exitCode: 128,
			message:  "remote: Write access to repository not granted.\nfatal: unable to access 'https://example.test/repo.git/': The requested URL returned error: 403",
			want:     TierGit,
		},
		{
			name:     "askpass helper cannot exec",
			exitCode: 128,
			message:  "fatal: cannot exec 'gaggles/site/workcopies/auth/goobers-askpass.sh': No such file or directory",
			want:     TierGit,
		},
		{
			name:     "missing ref",
			exitCode: 128,
			message:  "fatal: couldn't find remote ref refs/heads/missing",
			want:     TierGit,
		},
		{
			// Retryable (IsTransientProvisionError) but not provably a
			// network fault, so it stays git-owned rather than inflating the
			// network count an operator pages on.
			name:     "promisor backfill",
			exitCode: 128,
			message:  "fatal: could not fetch ce01362 from promisor remote",
			want:     TierGit,
		},
		{
			// Exit code does not gate classification the way it gates
			// retryability: a non-128 git failure still has an owner.
			name:     "network text on a non-128 exit",
			exitCode: 1,
			message:  "fatal: Could not resolve host: example.test",
			want:     TierNetwork,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &gitCommandError{exitCode: tt.exitCode, output: []byte(tt.message)}
			if got := ClassifyProvisionError(fmt.Errorf("create worktree: %w", err)); got != tt.want {
				t.Fatalf("ClassifyProvisionError(%q) = %q, want %q", tt.message, got, tt.want)
			}
		})
	}
}

// TestClassifyProvisionErrorStructuralOnly proves classification never fires
// on message text alone: an untyped error whose words look like a network
// failure stays unclassified, and typed non-git transport failures (the
// object cache's plain HTTP path) are recognized without git involvement.
func TestClassifyProvisionErrorStructuralOnly(t *testing.T) {
	if got := ClassifyProvisionError(errors.New("some other failure: connection refused")); got != TierUnknown {
		t.Fatalf("ClassifyProvisionError(untyped) = %q, want %q", got, TierUnknown)
	}
	if got := ClassifyProvisionError(nil); got != TierUnknown {
		t.Fatalf("ClassifyProvisionError(nil) = %q, want %q", got, TierUnknown)
	}
	dns := fmt.Errorf("refresh object cache: %w", &net.DNSError{Err: "no such host", Name: "example.test"})
	if got := ClassifyProvisionError(dns); got != TierNetwork {
		t.Fatalf("ClassifyProvisionError(DNSError) = %q, want %q", got, TierNetwork)
	}
}
