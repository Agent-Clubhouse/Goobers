package temporalcodec

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/proto"

	"github.com/goobers/goobers/internal/instance"
)

type testKeys struct {
	versions       map[string]*rsa.PrivateKey
	active         string
	wraps, unwraps int
	fail           bool
}

func (k *testKeys) Wrap(ctx context.Context, ref instance.KeyRef, plaintext []byte) ([]byte, string, error) {
	k.wraps++
	if k.fail || ctx.Err() != nil {
		return nil, "", errors.New("sensitive backend response")
	}
	version := ref.Version
	if version == "" {
		version = k.active
	}
	key := k.versions[version]
	if key == nil {
		return nil, "", errors.New("missing key")
	}
	data, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &key.PublicKey, plaintext, nil)
	return data, version, err
}
func (k *testKeys) Unwrap(ctx context.Context, ref instance.KeyRef, ciphertext []byte) ([]byte, error) {
	k.unwraps++
	if k.fail || ctx.Err() != nil {
		return nil, errors.New("sensitive backend response")
	}
	key := k.versions[ref.Version]
	if key == nil {
		return nil, errors.New("missing key")
	}
	return rsa.DecryptOAEP(sha256.New(), rand.Reader, key, ciphertext, nil)
}
func newTestCodec(t *testing.T) (*Codec, *testKeys) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := &testKeys{versions: map[string]*rsa.PrivateKey{"v1": key}, active: "v1"}
	codec, err := New(keys, instance.KeyRef{Store: "keys", Name: "history"}, false)
	if err != nil {
		t.Fatal(err)
	}
	return codec, keys
}
func testPayload(t *testing.T) *commonpb.Payload {
	t.Helper()
	p, err := converter.GetDefaultDataConverter().ToPayload("sensitive goal and instructions")
	if err != nil {
		t.Fatal(err)
	}
	p.Metadata["sensitive-original-metadata"] = []byte("sensitive metadata value")
	return p
}

func TestCodecSealsAllContentAndUsesFreshDataKeys(t *testing.T) {
	codec, keys := newTestCodec(t)
	plain := testPayload(t)
	sealed, err := codec.Encode([]*commonpb.Payload{plain, plain})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range sealed {
		if string(p.Metadata["encoding"]) != Encoding || string(p.Metadata[keyVersionField]) != "v1" {
			t.Fatal("missing sealed metadata")
		}
		serialized, _ := proto.Marshal(p)
		if bytes.Contains(serialized, []byte("sensitive")) {
			t.Fatal("original content escaped envelope")
		}
	}
	if bytes.Equal(sealed[0].Metadata[wrappedKeyField], sealed[1].Metadata[wrappedKeyField]) || bytes.Equal(sealed[0].Data, sealed[1].Data) {
		t.Fatal("reused sealed material")
	}
	got, err := codec.Decode(sealed)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if !proto.Equal(p, plain) {
			t.Fatal("roundtrip lost original payload or metadata")
		}
	}
	if _, err := codec.Decode(sealed); err != nil {
		t.Fatal(err)
	}
	if keys.unwraps != 4 {
		t.Fatal("unwrapped material was cached")
	}
}

func TestCodecRotationAndMixedHistories(t *testing.T) {
	codec, keys := newTestCodec(t)
	plain := testPayload(t)
	old, err := codec.Encode([]*commonpb.Payload{plain})
	if err != nil {
		t.Fatal(err)
	}
	next, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys.versions["v2"] = next
	keys.active = "v2"
	fresh, err := codec.Encode([]*commonpb.Payload{plain})
	if err != nil {
		t.Fatal(err)
	}
	if string(fresh[0].Metadata[keyVersionField]) != "v2" {
		t.Fatal("rotation ignored")
	}
	// A new configured wrap pin must still read retained old backend versions.
	codec.ref.Version = "v2"
	mixed := []*commonpb.Payload{old[0], plain, fresh[0]}
	decoded, err := codec.Decode(mixed)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range decoded {
		if !proto.Equal(p, plain) {
			t.Fatal("mixed history decode mismatch")
		}
	}
	codec.strict = true
	if decoded, err := codec.Decode(mixed); err == nil || decoded != nil {
		t.Fatal("strict accepted legacy plaintext")
	}
	if _, err := codec.Decode([]*commonpb.Payload{old[0], fresh[0]}); err != nil {
		t.Fatal(err)
	}
	delete(keys.versions, "v1")
	if _, err := codec.Decode(old); !errors.Is(err, ErrInvalidPayload) {
		t.Fatal("missing version did not fail closed")
	}
}

func TestCodecRejectsTamperingAndWrongKeys(t *testing.T) {
	codec, keys := newTestCodec(t)
	sealed, err := codec.Encode([]*commonpb.Payload{testPayload(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"data", "encoding", keyStoreField, keyNameField, keyVersionField, wrappedKeyField, nonceField, "extra", "missing-encoding"} {
		t.Run(field, func(t *testing.T) {
			p := proto.Clone(sealed[0]).(*commonpb.Payload)
			switch field {
			case "data":
				p.Data[0] ^= 1
			case "extra":
				p.Metadata["injected"] = []byte("value")
			case "missing-encoding":
				delete(p.Metadata, "encoding")
			default:
				p.Metadata[field][0] ^= 1
			}
			if got, err := codec.Decode([]*commonpb.Payload{p}); !errors.Is(err, ErrInvalidPayload) || got != nil {
				t.Fatal("accepted tampered payload")
			}
		})
	}
	wrong, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys.versions["v1"] = wrong
	if _, err := codec.Decode(sealed); !errors.Is(err, ErrInvalidPayload) {
		t.Fatal("accepted wrong key")
	}
	keys.fail = true
	if _, err := codec.Encode([]*commonpb.Payload{testPayload(t)}); !errors.Is(err, ErrInvalidPayload) {
		t.Fatal("backend error leaked")
	}
}

func TestCodecBoundsAndCancellation(t *testing.T) {
	codec, keys := newTestCodec(t)
	cases := [][]*commonpb.Payload{{nil}, make([]*commonpb.Payload, MaxBatchPayloads+1), {{Data: make([]byte, MaxPayloadBytes+1)}}}
	large := &commonpb.Payload{Data: make([]byte, MaxPayloadBytes-4)}
	cases = append(cases, []*commonpb.Payload{large, large, large, large, large})
	for _, batch := range cases {
		if _, err := codec.Encode(batch); err == nil {
			t.Fatal("accepted oversized or nil batch")
		}
	}
	if keys.wraps != 0 {
		t.Fatal("bounds checked after key operations")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := codec.EncodeContext(ctx, []*commonpb.Payload{testPayload(t)}); err == nil {
		t.Fatal("ignored cancellation")
	}
	// The sealed representation may grow past the plaintext batch cap.
	encoded, err := codec.Encode([]*commonpb.Payload{large, large, large, large})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Decode(encoded); err != nil {
		t.Fatalf("cannot decode maximum allowed batch: %v", err)
	}
}

func TestDefaultConverterIsByteIdentical(t *testing.T) {
	for _, cfg := range []*instance.Config{nil, {}, {Temporal: &instance.TemporalConfig{PayloadCodec: &instance.PayloadCodecConfig{}}}} {
		dc, err := DataConverter(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if dc != converter.GetDefaultDataConverter() {
			t.Fatal("unset codec changed default converter")
		}
		got, err := dc.ToPayload("legacy content")
		if err != nil {
			t.Fatal(err)
		}
		want, err := converter.GetDefaultDataConverter().ToPayload("legacy content")
		if err != nil {
			t.Fatal(err)
		}
		gotBytes, _ := proto.Marshal(got)
		wantBytes, _ := proto.Marshal(want)
		if !bytes.Equal(gotBytes, wantBytes) {
			t.Fatal("unset codec changed payload bytes")
		}
	}
	if _, err := FromConfig(nil); err == nil {
		t.Fatal("constructed plaintext remote codec")
	}
}

func TestCodecRejectsMalformedHeadersBeforeUnwrap(t *testing.T) {
	codec, keys := newTestCodec(t)
	sealed, err := codec.Encode([]*commonpb.Payload{testPayload(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*commonpb.Payload){
		func(p *commonpb.Payload) { p.Metadata[nonceField] = nil },
		func(p *commonpb.Payload) { p.Metadata[nonceField] = make([]byte, 13) },
		func(p *commonpb.Payload) { p.Metadata[keyVersionField] = nil },
		func(p *commonpb.Payload) { p.Metadata[wrappedKeyField] = nil },
		func(p *commonpb.Payload) { p.Metadata[wrappedKeyField] = make([]byte, 1025) },
		func(p *commonpb.Payload) { p.Data = make([]byte, 15) },
	} {
		p := proto.Clone(sealed[0]).(*commonpb.Payload)
		change(p)
		if got, err := codec.Decode([]*commonpb.Payload{p}); err == nil || got != nil {
			t.Fatal("accepted malformed header")
		}
	}
	if keys.unwraps != 0 {
		t.Fatal("malformed header reached key service")
	}
}
