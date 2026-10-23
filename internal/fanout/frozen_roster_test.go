package fanout

import (
	"encoding/json"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestFreezeRosterBlobCanonicalizesAndVerifiesSource(t *testing.T) {
	def, source, data := frozenRosterFixture()
	blob, encoded, digest, err := FreezeRosterBlob(def, source, data)
	if err != nil {
		t.Fatal(err)
	}
	if digest != journal.Digest(encoded) || blob.SchemaVersion != FrozenRosterSchemaVersion {
		t.Fatalf("frozen blob identity = %q, schema = %q", digest, blob.SchemaVersion)
	}
	if blob.Roster.Source != source || blob.Roster.MaxConcurrent != 2 ||
		blob.Roster.FailurePolicy != apiv1.BranchContinueOnError ||
		blob.Roster.ItemTimeoutSeconds != 30 {
		t.Fatalf("frozen roster metadata = %+v", blob.Roster)
	}
	if len(blob.Roster.Items) != 2 ||
		blob.Roster.Items[0].Identity != "a" ||
		string(blob.Roster.Items[0].Item) != `{"shard":"a"}` ||
		blob.Roster.Items[1].Identity != "b" {
		t.Fatalf("frozen roster items = %+v", blob.Roster.Items)
	}
	decoded, err := DecodeRosterBlob(encoded, digest, def)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Roster.Items) != 2 || decoded.Roster.Items[0].Digest == "" {
		t.Fatalf("decoded frozen roster = %+v", decoded)
	}
}

func TestFreezeRosterBlobRejectsSourceSubstitution(t *testing.T) {
	def, source, data := frozenRosterFixture()
	tests := map[string]func(*journal.FanOutSourceRef, *[]byte){
		"source task": func(source *journal.FanOutSourceRef, _ *[]byte) { source.Task = "other" },
		"visit":       func(source *journal.FanOutSourceRef, _ *[]byte) { source.Visit = 0 },
		"attempt":     func(source *journal.FanOutSourceRef, _ *[]byte) { source.Attempt = 0 },
		"media type":  func(source *journal.FanOutSourceRef, _ *[]byte) { source.MediaType = "text/plain" },
		"size":        func(source *journal.FanOutSourceRef, _ *[]byte) { source.Size++ },
		"digest":      func(source *journal.FanOutSourceRef, _ *[]byte) { source.Digest = journal.Digest([]byte("other")) },
		"bytes":       func(_ *journal.FanOutSourceRef, data *[]byte) { (*data)[0] = 'x' },
		"integrity":   func(source *journal.FanOutSourceRef, _ *[]byte) { source.Integrity = "forged" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			gotSource := source
			gotData := append([]byte(nil), data...)
			mutate(&gotSource, &gotData)
			if _, _, _, err := FreezeRosterBlob(def, gotSource, gotData); err == nil {
				t.Fatal("FreezeRosterBlob accepted substituted source")
			}
		})
	}
}

func TestDecodeRosterBlobRejectsNonCanonicalOrCorruptContent(t *testing.T) {
	def, source, data := frozenRosterFixture()
	blob, encoded, digest, err := FreezeRosterBlob(def, source, data)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	indented, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRosterBlob(indented, journal.Digest(indented), def); err == nil {
		t.Fatal("DecodeRosterBlob accepted non-canonical encoding")
	}

	blob.Roster.Items[0].Item = json.RawMessage(`{"shard":"forged"}`)
	corrupt, err := json.Marshal(blob)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeRosterBlob(corrupt, journal.Digest(corrupt), def); err == nil {
		t.Fatal("DecodeRosterBlob accepted item bytes that disagree with the item digest")
	} else {
		var integrity *IntegrityError
		if !errors.As(err, &integrity) {
			t.Fatalf("corrupt roster error = %T %v, want IntegrityError", err, err)
		}
	}
	if _, err := DecodeRosterBlob(encoded, journal.Digest([]byte("wrong")), def); err == nil {
		t.Fatal("DecodeRosterBlob accepted a mismatched blob digest")
	}
	_ = digest
}

func frozenRosterFixture() (apiv1.Map, journal.FanOutSourceRef, []byte) {
	data := []byte(`{"schemaVersion":1,"items":[{"identity":"b","item":{"shard":"b"}},{"identity":"a","item":{"shard":"a"}}]}`)
	def := apiv1.Map{
		Name: "shards", Source: apiv1.FanOutSource{Task: "plan", Slot: "roster"}, Task: "work",
		FailurePolicy: apiv1.BranchContinueOnError, Join: "collate",
		ItemTimeoutSeconds: 30, MaxConcurrent: 2,
	}
	source := journal.FanOutSourceRef{
		Task: "plan", Slot: "roster", Visit: 7, Attempt: 2,
		Digest: journal.Digest(data), Integrity: apiv1.IntegrityMaintainer,
		Path: "artifacts/roster.json", Size: int64(len(data)), MediaType: SourceMediaType,
	}
	return def, source, data
}
