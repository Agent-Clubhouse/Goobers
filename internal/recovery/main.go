package recovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"
)

// FetchCurrentBase resolves baseRef by fetching the caller-verified repository
// URL, not a cached tracking branch. The caller must bind that URL to the
// retained record's repository identity and supply only trusted Git credential
// environment entries. It leaves HEAD, the index and FETCH_HEAD unchanged.
// The result is the base as observed by this fetch, not a lease on the remote ref.
func FetchCurrentBase(ctx context.Context, repository, remoteURL, baseRef string, credentialEnvironment []string) (string, error) {
	if err := validateCredentialEnvironment(credentialEnvironment, remoteURL); err != nil {
		return "", err
	}
	if remoteURL == "" || strings.HasPrefix(remoteURL, "-") || strings.ContainsAny(remoteURL, "\x00\r\n") {
		return "", fmt.Errorf("invalid recovery base remote")
	}
	if !validRecoveryBaseRef(baseRef) {
		return "", fmt.Errorf("invalid recovery base ref")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	ref := "refs/goobers/recovery-fetch/" + hex.EncodeToString(nonce[:])
	defer func() {
		// Cancellation must not leave temporary fetch roots indefinitely. This
		// exact random ref belongs only to this operation; never prune a prefix.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = recoveryGit(cleanup, repository, io.Discard, "update-ref", "--no-deref", "-d", ref)
	}()
	if err := recoveryGitWithEnv(ctx, repository, io.Discard, credentialEnvironment,
		"fetch", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--",
		remoteURL, baseRef+":"+ref); err != nil {
		return "", fmt.Errorf("fetch current recovery base %q: %w", baseRef, err)
	}
	var output boundedRefOutput
	if err := recoveryGit(ctx, repository, &output, "rev-parse", "--verify", ref+"^{commit}"); err != nil {
		return "", fmt.Errorf("resolve fetched recovery base %q: %w", baseRef, err)
	}
	sha := strings.TrimSpace(output.String())
	if !gitObjectID.MatchString(sha) {
		return "", fmt.Errorf("invalid fetched base identity")
	}
	return sha, nil
}
