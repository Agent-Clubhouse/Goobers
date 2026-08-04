package providerscontract

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/providerfixture"
)

func TestContract_GitHubRecordedFixture(t *testing.T) {
	for _, name := range []string{"github_contract.json", "github_pr_contract.json"} {
		t.Run(name, func(t *testing.T) {
			fixture, err := providerfixture.Read(filepath.Join("testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			if err := providerfixture.CheckContract(context.Background(), fixture); err != nil {
				t.Fatal(err)
			}
		})
	}
}
