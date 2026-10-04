package sessioning

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/workbench"
)

func TestResolutionOperationRejectsNestedAuthorityAndDuplicateEvidenceKeys(t *testing.T) {
	ref := workbench.NeedsHumanEvidenceRef{Kind: "current-human-message", ID: "message-one", Digest: strings.Repeat("a", 64)}
	request := NeedsHumanResolutionRequest{SourceBindingID: "items", RequestID: "resolution-one", NeedsHumanResolutionRequest: workbench.NeedsHumanResolutionRequest{ID: "42", SourceID: "9876", ExpectedRevision: "rev", ObservationDigest: strings.Repeat("b", 64), Basis: ref, Rationale: "The answer resolves the question.", Evidence: []workbench.NeedsHumanEvidenceRef{ref}}}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeNeedsHumanResolution(raw); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(string(raw), `"basis":{`, `"basis":{"actor":"forged",`, 1), strings.Replace(string(raw), `"kind":"current-human-message"`, `"kind":"current-human-message","kind":"learned-record"`, 1), strings.Replace(string(raw), `"evidence":[{`, `"evidence":[{"Digest":"forged",`, 1), strings.Replace(string(raw), `"rationale":`, `"origin":{"runId":"forged"},"rationale":`, 1)} {
		if _, err = DecodeNeedsHumanResolution([]byte(bad)); err == nil {
			t.Fatal("open nested assessment", bad)
		}
	}
	if _, err = DecodeNeedsHumanInspect([]byte(`{"sourceBindingId":"items","id":"42"}`)); err == nil {
		t.Fatal("unbound source identity")
	}
	if _, err = DecodeNeedsHumanReceipt([]byte(`{"sourceBindingId":"items","commandId":"workbench-` + strings.Repeat("a", 32) + `"}`)); err == nil {
		t.Fatal("generic field receipt adopted")
	}
}
