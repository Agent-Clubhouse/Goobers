package childpod

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
)

func TestContractRejectsPublicationAndSubstitution(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	id := journal.RunIdentity{Schema: journal.RunSchema, InstanceID: "instance", RunID: strings.Repeat("1", 32), Workflow: "generated", Gaggle: "gaggle", ConfigGeneration: digest, WorkflowDigest: digest, GooberDigest: digest, StartedAt: time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)}
	id.Child = &journal.ChildLineage{Gaggle: id.Gaggle, ParentRunID: strings.Repeat("2", 32), ParentWorkflow: "parent", StageOccurrence: "occurrence", InvocationKey: "key", AcceptanceID: "trigger-" + id.RunID, SourceDigest: digest, EnvelopeDigest: digest}
	c := Contract{Version: 1, Identity: id, Stage: "stage", Attempt: 1, PodAttempt: 11, StartedAt: id.StartedAt, Ceiling: credentials.NewChildCeiling(false, nil, nil)}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeContract(data, journal.Digest(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeContract(data, journal.Digest([]byte("foreign"))); err == nil {
		t.Fatal("substitution accepted")
	}
	c.Ceiling.AllowPublication = true
	if c.Validate() == nil {
		t.Fatal("synthetic history publication accepted")
	}
}
