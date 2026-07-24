package providers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// TestADOAzureCLILive is opt-in because it requires an authenticated Azure CLI
// session and access to the named repository. The environment value is
// organization/project/repository.
func TestADOAzureCLILive(t *testing.T) {
	target := os.Getenv("GOOBERS_ADO_LIVE_REPO")
	if target == "" {
		t.Skip("GOOBERS_ADO_LIVE_REPO is not set")
	}
	parts := strings.Split(target, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		t.Fatal("GOOBERS_ADO_LIVE_REPO must be organization/project/repository")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	source := NewAzureCLIADOCredentialSource(nil, os.Getenv("GOOBERS_ADO_TENANT"))
	provider := NewADOProvider(parts[0], parts[1], "",
		WithADOCredentialSource(source),
		WithADOSecretRegistrar(journal.NewRegistryScrubber()),
	)
	if err := provider.RepositoryReachable(ctx, RepositoryRef{
		Provider: ProviderADO,
		Owner:    parts[0],
		Project:  parts[1],
		Name:     parts[2],
	}); err != nil {
		t.Fatal(err)
	}
}
