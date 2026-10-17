// Package publicationguard checks every commit being published, including
// intermediate trees that disappear from a pull request's final diff.
package publicationguard

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/goobers/goobers/internal/publicationpaths"
	"github.com/goobers/goobers/internal/secretpattern"
)

// Git executes a bounded, local Git read. Errors must not contain command
// output: blob contents and even filenames may contain credentials.
type Git func(context.Context, ...string) ([]byte, error)

// Check fails closed when the outgoing history cannot be inspected. Deletions
// are allowed so a repository can remove previously committed artifacts. A
// later deletion never excuses an artifact introduced in an outgoing commit.
// The returned commit ID is the inspected tip; callers must push this exact ID.
func Check(ctx context.Context, git Git, base, head string) (string, error) {
	shallow, err := git(ctx, "rev-parse", "--is-shallow-repository")
	if err != nil || strings.TrimSpace(string(shallow)) != "false" {
		return "", fmt.Errorf("publication safety: complete local history is required; unshallow the checkout before publishing")
	}

	baseSHA, err := resolve(ctx, git, base)
	if err != nil {
		return "", err
	}
	headSHA, err := resolve(ctx, git, head)
	if err != nil {
		return "", err
	}
	commits, err := git(ctx, "rev-list", headSHA, "^"+baseSHA, "--")
	if err != nil {
		return "", fmt.Errorf("publication safety: enumerate outgoing history: %w", err)
	}
	seen := make(map[string]bool)
	for _, commit := range strings.Fields(string(commits)) {
		if !objectID.MatchString(commit) {
			return "", fmt.Errorf("publication safety: invalid commit inventory")
		}
		if err := checkCommit(ctx, git, commit, seen); err != nil {
			return "", err
		}
	}
	return headSHA, nil
}

var objectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func resolve(ctx context.Context, git Git, ref string) (string, error) {
	out, err := git(ctx, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("publication safety: cannot resolve publication revision: %w", err)
	}
	sha := strings.TrimSpace(string(out))
	if !objectID.MatchString(sha) {
		return "", fmt.Errorf("publication safety: invalid publication revision")
	}
	return sha, nil
}

func checkCommit(ctx context.Context, git Git, commit string, seen map[string]bool) error {
	// -m checks every parent of a merge; --no-renames exposes the destination
	// of a rename independently. NUL framing preserves unusual filenames.
	out, err := git(ctx, "diff-tree", "--root", "-r", "-m", "--no-commit-id", "--no-renames", "--raw", "-z", "--diff-filter=ACMT", commit, "--")
	if err != nil {
		return fmt.Errorf("publication safety: inspect outgoing tree: %w", err)
	}
	fields := bytes.Split(out, []byte{0})
	for len(fields) > 1 {
		if len(fields) < 3 {
			return fmt.Errorf("publication safety: incomplete tree inventory")
		}
		header, path := strings.Fields(string(fields[0])), string(fields[1])
		fields = fields[2:]
		if len(header) != 5 || !strings.HasPrefix(header[0], ":") || !objectID.MatchString(header[3]) || path == "" {
			return fmt.Errorf("publication safety: invalid tree inventory")
		}
		if publicationpaths.IsReserved(path) {
			return fmt.Errorf("publication safety: commit %s introduces or changes reserved harness artifacts; remove them from outgoing history before publishing", commit)
		}
		if header[1] == "160000" {
			continue
		} // submodule contents are a separate repository
		oid := header[3]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		blob, err := git(ctx, "cat-file", "blob", oid)
		if err != nil {
			return fmt.Errorf("publication safety: inspect outgoing blob: %w", err)
		}
		if ContainsCredential(blob) {
			return fmt.Errorf("publication safety: commit %s contains credential-shaped material; inspect privately and remove it from outgoing history before publishing (values withheld)", commit)
		}
	}
	if len(fields) != 1 || len(fields[0]) != 0 {
		return fmt.Errorf("publication safety: malformed tree inventory")
	}
	return nil
}

var providerSecrets = secretpattern.NewScrubber()

// Environment dumps include opaque connection credentials with no provider
// prefix. Match quoted JSON/JS and shell/environment assignments; placeholders
// and code references do not meet the long, literal-value requirement.
var credentialAssignment = regexp.MustCompile(`(?i)(?:GH_TOKEN|GITHUB_TOKEN|COPILOT_GITHUB_TOKEN|COPILOT_CONNECTION_TOKEN)["']?\s*[:=]\s*["']?[A-Za-z0-9_./+~=-]{16,}`)

// ContainsCredential detects the shared provider-token shapes and literal
// credential environment dumps. It returns no matching values or snippets.
// This is a deterministic backstop, not a detector for every possible secret.
func ContainsCredential(data []byte) bool {
	return credentialAssignment.Match(data) || !bytes.Equal(data, providerSecrets.Scrub(data))
}
