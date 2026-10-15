package fanout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/jcs"
	"github.com/goobers/goobers/internal/workflow"
)

// knownDigest pins the canonical digest of one item as an interoperability
// vector: sha256 over the RFC 8785 bytes
// {"identity":"unit-a","item":{"packages":["./internal/a/..."]}}.
const knownDigest = "sha256:42cca116643bcea25c63b826f0af53e9785c78b8ecac8483d1601243cfb0fa7f"

func TestHostBoundsMatchTheCompiler(t *testing.T) {
	if MaxSourceBytes != workflow.MaxFanOutSourceBytes {
		t.Fatalf("MaxSourceBytes = %d, compiler bound is %d", MaxSourceBytes, workflow.MaxFanOutSourceBytes)
	}
}

func TestItemDigestKnownAnswer(t *testing.T) {
	got, err := ItemDigest("unit-a", json.RawMessage(` { "packages" : [ "./internal/a/..." ] } `))
	if err != nil {
		t.Fatal(err)
	}
	if got != knownDigest {
		t.Fatalf("ItemDigest = %s, want %s", got, knownDigest)
	}
	if _, err := ItemDigest("unit-a", json.RawMessage(`["not","an","object"]`)); err == nil {
		t.Fatal("a non-object item must not have a digest")
	}
}

func roster(entries ...string) []byte {
	return []byte(`{"schemaVersion":1,"items":[` + strings.Join(entries, ",") + `]}`)
}

func TestFreezeRosterNormalizesOrderAndComputesDigests(t *testing.T) {
	forward := roster(
		`{"identity":"unit-b","item":{"packages":["./internal/b/..."]}}`,
		`{"identity":"unit-a","item":{"packages":["./internal/a/..."]},"digest":"`+knownDigest+`"}`,
		`{"identity":"Unit-C","item":{}}`,
	)
	reordered := []byte(`{ "items": [
		{"item":{},"identity":"Unit-C"},
		{"identity":"unit-a","item":{ "packages":["./internal/a/..."] }},
		{"identity":"unit-b","item":{"packages":["./internal/b/..."]}}
	], "schemaVersion": 1 }`)
	a, err := FreezeRoster(forward, SourceMediaType)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FreezeRoster(reordered, SourceMediaType)
	if err != nil {
		t.Fatal(err)
	}
	gotA, _ := json.Marshal(a)
	gotB, _ := json.Marshal(b)
	if string(gotA) != string(gotB) {
		t.Fatalf("array order or spelling changed the normalized roster:\n%s\n%s", gotA, gotB)
	}
	// Bytewise UTF-8 order puts the uppercase identity first.
	var identities []string
	for _, item := range a {
		identities = append(identities, item.Identity)
	}
	if strings.Join(identities, ",") != "Unit-C,unit-a,unit-b" {
		t.Fatalf("identities = %v, want bytewise order", identities)
	}
	if a[1].Digest != knownDigest || string(a[1].Item) != `{"packages":["./internal/a/..."]}` {
		t.Fatalf("item = %+v, want canonical item and its known digest", a[1])
	}
	for _, item := range a {
		want, _ := ItemDigest(item.Identity, item.Item)
		if item.Digest != want {
			t.Fatalf("item %q digest %s, want computed %s", item.Identity, item.Digest, want)
		}
	}
}

func TestFreezeRosterEmptyIsValid(t *testing.T) {
	items, err := FreezeRoster(roster(), SourceMediaType)
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("empty roster = %#v, %v; want an empty, non-nil roster", items, err)
	}
}

// Design §14 case 12: duplicate identities fail before map.started.
func TestFreezeRosterRejectsDuplicateIdentities(t *testing.T) {
	_, err := FreezeRoster(roster(`{"identity":"unit-a","item":{"x":1}}`, `{"identity":"unit-a","item":{"x":2}}`), SourceMediaType)
	assertSourceError(t, err, ReasonDuplicateIdentity, "unit-a")
}

// Design §14 case 13: a malformed or mismatched supplied digest fails, an
// omitted digest is computed, and verification excludes the digest field.
func TestFreezeRosterVerifiesSuppliedDigests(t *testing.T) {
	item := `{"packages":["./internal/a/..."]}`
	t.Run("omitted digest is computed", func(t *testing.T) {
		items, err := FreezeRoster(roster(`{"identity":"unit-a","item":`+item+`}`), SourceMediaType)
		if err != nil || items[0].Digest != knownDigest {
			t.Fatalf("items = %+v, %v; want computed %s", items, err, knownDigest)
		}
	})
	t.Run("matching supplied digest is accepted", func(t *testing.T) {
		if _, err := FreezeRoster(roster(`{"identity":"unit-a","item":`+item+`,"digest":"`+knownDigest+`"}`), SourceMediaType); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("mismatched supplied digest", func(t *testing.T) {
		other := "sha256:" + strings.Repeat("0", 64)
		_, err := FreezeRoster(roster(`{"identity":"unit-a","item":`+item+`,"digest":"`+other+`"}`), SourceMediaType)
		assertSourceError(t, err, ReasonDigestMismatch, "unit-a")
	})
	t.Run("digest of another identity", func(t *testing.T) {
		_, err := FreezeRoster(roster(`{"identity":"unit-b","item":`+item+`,"digest":"`+knownDigest+`"}`), SourceMediaType)
		assertSourceError(t, err, ReasonDigestMismatch, "unit-b")
	})
	t.Run("digest field is outside the hashed value", func(t *testing.T) {
		// A digest computed over the whole entry, digest field included, is
		// not the canonical digest and must be refused.
		self, err := jcs.Canonicalize([]byte(`{"identity":"unit-a","item":` + item + `,"digest":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(self)
		selfDigest := "sha256:" + hex.EncodeToString(sum[:])
		_, err = FreezeRoster(roster(`{"identity":"unit-a","item":`+item+`,"digest":"`+selfDigest+`"}`), SourceMediaType)
		assertSourceError(t, err, ReasonDigestMismatch, "unit-a")
	})
	for name, digest := range map[string]string{
		"uppercase hex":   "sha256:" + strings.Repeat("A", 64),
		"short":           "sha256:abc",
		"wrong algorithm": "sha512:" + strings.Repeat("0", 64),
		"bare hex":        strings.Repeat("0", 64),
	} {
		t.Run("malformed digest/"+name, func(t *testing.T) {
			_, err := FreezeRoster(roster(`{"identity":"unit-a","item":`+item+`,"digest":"`+digest+`"}`), SourceMediaType)
			// The closed envelope pins the digest pattern, so a malformed
			// digest is refused there, before any hashing.
			assertSourceError(t, err, ReasonEnvelope, "")
		})
	}
}

func TestFreezeRosterRejectsInvalidSources(t *testing.T) {
	tooMany := make([]string, MaxRosterItems+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf(`{"identity":"item-%02d","item":{}}`, i)
	}
	for _, tc := range []struct {
		name, mediaType, reason string
		data                    []byte
	}{
		{"wrong media type", "text/plain", ReasonMediaType, roster()},
		// Size is checked before decoding: oversized garbage is too_large,
		// not malformed.
		{"oversized before decoding", SourceMediaType, ReasonTooLarge, []byte(strings.Repeat("x", int(MaxSourceBytes)+1))},
		{"not JSON", SourceMediaType, ReasonEnvelope, []byte(`{"schemaVersion":1,`)},
		{"missing schemaVersion", SourceMediaType, ReasonEnvelope, []byte(`{"items":[]}`)},
		{"unknown envelope field", SourceMediaType, ReasonEnvelope, []byte(`{"schemaVersion":1,"items":[],"extra":true}`)},
		{"unknown item field", SourceMediaType, ReasonEnvelope, roster(`{"identity":"a","item":{},"reason":"x"}`)},
		{"duplicate JSON member", SourceMediaType, ReasonEnvelope, []byte(`{"schemaVersion":1,"schemaVersion":1,"items":[]}`)},
		{"item not an object", SourceMediaType, ReasonEnvelope, roster(`{"identity":"a","item":[1]}`)},
		{"identity charset", SourceMediaType, ReasonEnvelope, roster(`{"identity":"-leading-dash","item":{}}`)},
		{"identity too long", SourceMediaType, ReasonEnvelope, roster(`{"identity":"a` + strings.Repeat("b", 128) + `","item":{}}`)},
		{"too many items", SourceMediaType, ReasonEnvelope, roster(tooMany...)},
		{"item number overflow", SourceMediaType, ReasonItem, roster(`{"identity":"a","item":{"n":1e400}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FreezeRoster(tc.data, tc.mediaType)
			assertSourceError(t, err, tc.reason, "")
		})
	}
}

func assertSourceError(t *testing.T, err error, reason, identity string) {
	t.Helper()
	var typed *SourceError
	if !errors.Is(err, ErrSourceInvalid) || !errors.As(err, &typed) {
		t.Fatalf("error = %v, want a typed %s", err, CodeSourceInvalid)
	}
	if typed.Reason != reason || (identity != "" && typed.Identity != identity) {
		t.Fatalf("error = %+v, want reason %q identity %q", typed, reason, identity)
	}
	if typed.ErrorCode() != CodeSourceInvalid || typed.ErrorClass() != reason {
		t.Fatalf("typed code/class = %s/%s", typed.ErrorCode(), typed.ErrorClass())
	}
}
