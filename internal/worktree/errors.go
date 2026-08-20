package worktree

import (
	"errors"
	"net"
	"strings"
)

// FailureTier names which infrastructure tier owns a worktree-provisioning
// failure. Git exits 128 for a DNS outage, an unauthorized clone, and a
// missing ref alike, so the exit code cannot separate them — but the three
// need three different owners, and collapsing them into one bucket is exactly
// what made a gaggle's clone-403 regime indistinguishable from its DNS
// outage in telemetry until each run's journal text was read by hand.
type FailureTier string

const (
	// TierUnknown means err is not a recognized provisioning failure — the
	// caller keeps whatever classification it already had.
	TierUnknown FailureTier = ""
	// TierNetwork is a failure reaching the remote at all: DNS, connectivity,
	// transport timeout, or a remote 5xx.
	TierNetwork FailureTier = "network"
	// TierGit is a git-reported failure that DID reach the remote (or never
	// needed to): clone/fetch authorization, a missing ref, a broken
	// credential helper, a worktree already present.
	TierGit FailureTier = "git"
)

// ClassifyProvisionError separates the two owners of a worktree-provisioning
// failure so a caller can journal a typed error class instead of one opaque
// code. Unlike IsTransientProvisionError this asks "whose failure is it?",
// not "is retrying worth it?" — a remote 5xx is both network-owned and
// retryable, an unauthorized clone is git-owned and permanent, and a promisor
// backfill failure is retryable without being provably network-owned.
//
// Classification is structural: it requires the typed *gitCommandError (or a
// net error) rather than matching on any caller's formatted message. Only the
// git output text — git's own words, which carry no other machine-readable
// signal — is matched, and only to split network from non-network.
func ClassifyProvisionError(err error) FailureTier {
	if err == nil {
		return TierUnknown
	}
	var gitErr *gitCommandError
	if errors.As(err, &gitErr) {
		if isNetworkGitOutput(string(gitErr.output)) {
			return TierNetwork
		}
		return TierGit
	}
	// Object-cache and mirror maintenance reach the remote over plain HTTP
	// rather than through git, so their transport failures arrive typed.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return TierNetwork
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return TierNetwork
	}
	return TierUnknown
}

// networkOutputFragments are git's own phrasings for a failure to reach the
// remote. Shared by ClassifyProvisionError and IsTransientProvisionError so
// the two never drift apart on what "network" means.
var networkOutputFragments = []string{
	"could not resolve host",
	"couldn't resolve host",
	"failed to connect to",
	"could not connect to",
	"connection refused",
	"connection reset",
	"connection timed out",
	"ssl connection timeout",
	"empty reply from server",
	"network is unreachable",
	"operation timed out",
	"timeout was reached",
	"timed out after",
	"the remote end hung up unexpectedly",
	"unexpected disconnect",
	"early eof",
}

func isNetworkGitOutput(output string) bool {
	message := strings.ToLower(output)
	for _, fragment := range networkOutputFragments {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return remote5xxPattern.MatchString(message)
}
