package validate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/goobers/goobers/api/schemas"
)

func TestRunIdentitySchemaPreservesInstanceIdentity(t *testing.T) {
	contract := minimalHostedProgressContract(t)
	contract.Identity.RunID = strings.Repeat("a", 32)
	contract.Identity.InstanceID = strings.Repeat("b", 32)
	data, err := json.Marshal(contract.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := newV(t).ValidateJSON(schemas.Journal["run"], data); err != nil {
		t.Fatalf("run identity schema rejects producer's instance identity: %v", err)
	}
	data, err = json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	if err := newV(t).ValidateJSON(schemas.HostedProgress, data); err != nil {
		t.Fatalf("hosted projection rejects instance identity: %v", err)
	}
}
