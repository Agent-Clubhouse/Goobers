package fanout

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestFreezeRosterBlobDoesNotExpandHTMLCharacters(t *testing.T) {
	items := make([]string, 32)
	for i := range items {
		items[i] = fmt.Sprintf(`{"identity":"item-%02d","item":{"value":"%s"}}`,
			i, strings.Repeat("<", 30_000))
	}
	data := []byte(`{"schemaVersion":1,"items":[` + strings.Join(items, ",") + `]}`)
	def, source, _ := frozenRosterFixture()
	source.Digest, source.Size = journal.Digest(data), int64(len(data))
	_, encoded, digest, err := FreezeRosterBlob(def, source, data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `\u003c`) || int64(len(encoded)) > MaxFrozenRosterBytes {
		t.Fatalf("normalized roster expanded HTML characters: source=%d frozen=%d", len(data), len(encoded))
	}
	if _, err := DecodeRosterBlob(encoded, digest, def); err != nil {
		t.Fatalf("decode non-HTML-escaped roster: %v", err)
	}
}

func TestFreezeRosterBlobAppliesSharedNormalizedSizeBound(t *testing.T) {
	def, source, data := frozenRosterFixture()
	_, encoded, _, err := FreezeRosterBlob(def, source, data)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, make([]byte, int(MaxFrozenRosterBytes)-len(encoded)+1)...)
	_, err = DecodeRosterBlob(encoded, journal.Digest(encoded), def)
	var invalid *SourceError
	if !errors.As(err, &invalid) || invalid.Reason != ReasonTooLarge ||
		!strings.Contains(err.Error(), "normalized roster") {
		t.Fatalf("normalized oversize error = %T %v", err, err)
	}
}

func TestFreezeRosterBlobAllowsDocumentedNormalizedOverhead(t *testing.T) {
	data := []byte(`{"schemaVersion":1,"items":[{"identity":"a","item":{"value":"` +
		strings.Repeat("x", MaxFrozenItemBytes-len(`{"value":""}`)) + `"}}]}`)
	def, source, _ := frozenRosterFixture()
	source.Digest, source.Size = journal.Digest(data), int64(len(data))
	_, encoded, _, err := FreezeRosterBlob(def, source, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) <= len(data) || int64(len(encoded)) > MaxFrozenRosterBytes {
		t.Fatalf("normalized overhead source=%d frozen=%d bound=%d", len(data), len(encoded), MaxFrozenRosterBytes)
	}
}

func TestFreezeRosterBlobCanonicalizesAndVerifiesSource(t *testing.T) {
	def, source, data := frozenRosterFixture()
	blob, encoded, digest, err := FreezeRosterBlob(def, source, data)
	if err != nil {
		t.Fatal(err)
	}
	if digest != journal.Digest(encoded) || blob.SchemaVersion != FrozenRosterSchemaVersion {
		t.Fatalf("frozen blob identity = %q, schema = %q", digest, blob.SchemaVersion)
	}
	if blob.Roster.Source.Task != source.Task || blob.Roster.Source.Slot != source.Slot ||
		blob.Roster.Source.Visit != 0 || blob.Roster.Source.Path != "" ||
		blob.Roster.MaxConcurrent != 2 ||
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
