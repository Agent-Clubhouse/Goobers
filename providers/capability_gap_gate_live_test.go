//go:build integration

package providers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// TestIntegrationTrackedGapReferencesAreOpen is #3058's forge-aware gate:
// every GapTracked entry in providers/capability_matrix.go's knownGaps
// registry must still point at an open issue on the repository's forge. A
// failure means the registry rotted — the issue closed while the entry
// stayed, exactly the state that made ADO's pr.query.assignee cell link a
// closed issue for months.
//
// It lives behind the integration tag (and skips unless opted in) because
// unlike ValidateGapRegistry/ValidateBlessedTier it needs the forge:
// default `go test ./...` and the merge-tier gates stay hermetic, while
// the scheduled tracked-gap-references workflow runs it with a token.
//
// GOOBERS_GAP_REGISTRY_REPO is "owner/name" and defaults to this
// repository, which is where the registry's "#NNN" references resolve.
func TestIntegrationTrackedGapReferencesAreOpen(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_GITHUB_TOKEN", "GITHUB_TOKEN")

	token := os.Getenv("GOOBERS_GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	spec := os.Getenv("GOOBERS_GAP_REGISTRY_REPO")
	if spec == "" {
		spec = "Agent-Clubhouse/Goobers"
	}
	owner, name, ok := strings.Cut(spec, "/")
	if !ok || owner == "" || name == "" {
		t.Fatalf("GOOBERS_GAP_REGISTRY_REPO = %q, want owner/name", spec)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	repo := RepositoryRef{Provider: ProviderGitHub, Owner: owner, Name: name}
	provider := NewGitHubProvider(token)
	for _, err := range ValidateTrackedGapsOpen(ctx, provider, repo) {
		t.Error(err)
	}
	t.Logf("checked %d tracked gap reference(s) against %s", len(TrackedGapReferences()), spec)
}
