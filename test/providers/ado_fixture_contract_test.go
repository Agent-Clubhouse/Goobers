package providerscontract

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/providerfixture"
)

func TestContract_ADORecordedFixture(t *testing.T) {
	fixture, err := providerfixture.Read(filepath.Join("testdata", "ado_contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := providerfixture.CheckContract(context.Background(), fixture); err != nil {
		t.Fatal(err)
	}
}
