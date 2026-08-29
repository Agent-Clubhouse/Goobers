//go:build integration

package providers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// TestIntegrationADOLiveSmoke is an opt-in, read-only smoke test against a
// real Azure DevOps organization/project (CONF-4, #2077's scheduled ADO
// live leg). Fixture-backed tests (ado_test.go, ado_landing_test.go, the
// test/providers contract corpus) pin behavior against recorded API
// shapes; this exercises the genuine live API surface those fixtures can
// silently drift from.
//
// The //go:build integration tag excludes this from the default `go test
// ./...` and CI's regular gates; it only runs where a caller explicitly
// builds with -tags=integration (the scheduled workflow below, or a
// developer opting in locally). testdep.RequireEnv additionally skips
// (never fails) when the env var isn't set, so any other integration-tagged
// sweep that doesn't provision ADO creds is unaffected.
//
// GOOBERS_ADO_LIVE_REPO is "organization/project/repository". Auth prefers
// a PAT (GOOBERS_ADO_LIVE_TOKEN) since CI runners have no interactive
// Azure CLI session; GOOBERS_ADO_TENANT + an `az login` session is the
// fallback for a developer running this locally without a PAT.
func TestIntegrationADOLiveSmoke(t *testing.T) {
	testdep.RequireEnv(t, "GOOBERS_ADO_LIVE_REPO")
	target := os.Getenv("GOOBERS_ADO_LIVE_REPO")
	parts := strings.Split(target, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		t.Fatalf("GOOBERS_ADO_LIVE_REPO = %q, want organization/project/repository", target)
	}
	repo := RepositoryRef{Provider: ProviderADO, Owner: parts[0], Project: parts[1], Name: parts[2]}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var provider *ADOProvider
	if token := os.Getenv("GOOBERS_ADO_LIVE_TOKEN"); token != "" {
		provider = NewADOProvider(parts[0], parts[1], token,
			WithADOSecretRegistrar(journal.NewRegistryScrubber()))
	} else {
		source := NewAzureCLIADOCredentialSource(nil, os.Getenv("GOOBERS_ADO_TENANT"))
		provider = NewADOProvider(parts[0], parts[1], "",
			WithADOCredentialSource(source),
			WithADOSecretRegistrar(journal.NewRegistryScrubber()),
		)
	}

	if err := provider.RepositoryReachable(ctx, repo); err != nil {
		t.Fatalf("live RepositoryReachable: %v", err)
	}

	items, err := provider.ListWorkItems(ctx, ListWorkItemsRequest{
		Repository: repo, State: "open", Limit: 50,
	})
	if err != nil {
		t.Fatalf("live ListWorkItems: %v", err)
	}
	t.Logf("live smoke ok: %s/%s/%s reachable, %d open work item(s)", parts[0], parts[1], parts[2], len(items))
}
