package temporalcodec

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/secretstore"
)

const (
	// Encoding identifies the versioned AES-256-GCM envelope format.
	Encoding = "binary/goobers-aesgcm-v1"
	// MaxPayloadBytes bounds the serialized original payload, including metadata.
	MaxPayloadBytes = 2 << 20
	// MaxBatchPayloads bounds key operations per batch.
	MaxBatchPayloads = 64
	// MaxBatchBytes bounds aggregate serialized payload bytes per batch.
	MaxBatchBytes    = 8 << 20
	maxSealedBytes   = MaxPayloadBytes + 4096
	operationTimeout = 30 * time.Second
	keyStoreField    = "goobers-key-store"
	keyNameField     = "goobers-key-name"
	keyVersionField  = "goobers-key-version"
	wrappedKeyField  = "goobers-wrapped-key"
	nonceField       = "goobers-nonce"
)

// ErrInvalidPayload is deliberately content-free: codec failures never expose
// original payloads, plaintext data keys, ciphertext, or backend response bodies.
var ErrInvalidPayload = errors.New("temporal payload codec: invalid payload or key operation failed")

// Codec seals each payload with an independent random AES-256 data key and
// nonce. All original metadata is encrypted; every outer metadata field is AAD.
// The wrapping store and key name are pinned to config. Versions come from the
// backend and are persisted per payload so old versions remain replayable.
type Codec struct {
	keys   secretstore.KeyStore
	ref    instance.KeyRef
	strict bool
}

// New builds an inert codec over the provided key operations. No key material
// or unwrapped data keys are cached by the codec.
func New(keys secretstore.KeyStore, ref instance.KeyRef, strict bool) (*Codec, error) {
	if keys == nil {
		return nil, ErrInvalidPayload
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	return &Codec{keys: keys, ref: ref, strict: strict}, nil
}

// Encode implements Temporal's PayloadCodec contract.
func (c *Codec) Encode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	return c.EncodeContext(context.Background(), payloads)
}

// Decode implements Temporal's PayloadCodec contract. Legacy payloads pass
// unchanged when strict is false; mixed sealed/plain batches are supported.
func (c *Codec) Decode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	return c.DecodeContext(context.Background(), payloads)
}

// EncodeContext applies a single bounded deadline to the whole batch.
func (c *Codec) EncodeContext(ctx context.Context, payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	return c.transform(ctx, payloads, false)
}

// DecodeContext allows remote callers to cancel key operations on disconnect.
func (c *Codec) DecodeContext(ctx context.Context, payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	return c.transform(ctx, payloads, true)
}

func (c *Codec) transform(ctx context.Context, payloads []*commonpb.Payload, decode bool) ([]*commonpb.Payload, error) {
	if err := validateBatch(payloads, decode); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	out := make([]*commonpb.Payload, len(payloads))
	for i, p := range payloads {
		var err error
		if ctx.Err() != nil {
			return nil, ErrInvalidPayload
		}
		if decode {
			out[i], err = c.decode(ctx, p)
		} else {
			out[i], err = c.encode(ctx, p)
		}
		if err != nil || ctx.Err() != nil {
			return nil, ErrInvalidPayload
		}
	}
	if decode {
		if err := validateBatch(out, false); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func validateBatch(payloads []*commonpb.Payload, sealed bool) error {
	if len(payloads) > MaxBatchPayloads {
		return ErrInvalidPayload
	}
	maxSize := MaxPayloadBytes
	maxTotal := MaxBatchBytes
	if sealed {
		maxSize = maxSealedBytes
		maxTotal += MaxBatchPayloads * 4096
	}
	total := 0
	for _, p := range payloads {
		if p == nil {
			return ErrInvalidPayload
		}
		size := proto.Size(p)
		total += size
		if size > maxSize || total > maxTotal {
			return ErrInvalidPayload
		}
	}
	return nil
}

func (c *Codec) encode(ctx context.Context, p *commonpb.Payload) (*commonpb.Payload, error) {
	plaintext, err := proto.Marshal(p)
	if err != nil {
		return nil, ErrInvalidPayload
	}
	defer clear(plaintext)
	dek := make([]byte, 32)
	defer clear(dek)
	if _, err := rand.Read(dek); err != nil {
		return nil, ErrInvalidPayload
	}
	wrapped, version, err := c.keys.Wrap(ctx, c.ref, dek)
	if err != nil || !instance.ValidKeyComponent(version) || len(wrapped) == 0 || len(wrapped) > 1024 {
		return nil, ErrInvalidPayload
	}
	aead, err := dataCipher(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrInvalidPayload
	}
	result := &commonpb.Payload{Metadata: map[string][]byte{
		"encoding": []byte(Encoding), keyStoreField: []byte(c.ref.Store), keyNameField: []byte(c.ref.Name), keyVersionField: []byte(version), wrappedKeyField: wrapped, nonceField: nonce,
	}}
	aad, err := metadataAAD(result)
	if err != nil {
		return nil, err
	}
	result.Data = aead.Seal(nil, nonce, plaintext, aad)
	return result, nil
}

func (c *Codec) decode(ctx context.Context, p *commonpb.Payload) (*commonpb.Payload, error) {
	if string(p.Metadata["encoding"]) != Encoding {
		if c.strict || hasCodecMetadata(p) {
			return nil, ErrInvalidPayload
		}
		return p, nil
	}
	ref, err := c.payloadRef(p)
	if err != nil {
		return nil, err
	}
	dek, err := c.keys.Unwrap(ctx, ref, p.Metadata[wrappedKeyField])
	defer clear(dek)
	if err != nil || len(dek) != 32 {
		return nil, ErrInvalidPayload
	}
	aead, err := dataCipher(dek)
	if err != nil {
		return nil, err
	}
	aad, err := metadataAAD(p)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, p.Metadata[nonceField], p.Data, aad)
	defer clear(plaintext)
	if err != nil || len(plaintext) > MaxPayloadBytes {
		return nil, ErrInvalidPayload
	}
	result := &commonpb.Payload{}
	if err := proto.Unmarshal(plaintext, result); err != nil {
		return nil, ErrInvalidPayload
	}
	return result, nil
}

func (c *Codec) payloadRef(p *commonpb.Payload) (instance.KeyRef, error) {
	ref := instance.KeyRef{Store: string(p.Metadata[keyStoreField]), Name: string(p.Metadata[keyNameField]), Version: string(p.Metadata[keyVersionField])}
	if len(p.Metadata) != 6 || len(p.Metadata[nonceField]) != 12 || len(p.Metadata[wrappedKeyField]) == 0 || len(p.Metadata[wrappedKeyField]) > 1024 || len(p.Data) < 16 {
		return ref, ErrInvalidPayload
	}
	if ref.Store != c.ref.Store || ref.Name != c.ref.Name || ref.Version == "" {
		return ref, ErrInvalidPayload
	}
	if err := ref.Validate(); err != nil {
		return ref, ErrInvalidPayload
	}
	return ref, nil
}

func hasCodecMetadata(p *commonpb.Payload) bool {
	for _, name := range []string{keyStoreField, keyNameField, keyVersionField, wrappedKeyField, nonceField} {
		if _, ok := p.Metadata[name]; ok {
			return true
		}
	}
	return false
}
func metadataAAD(p *commonpb.Payload) ([]byte, error) {
	return (proto.MarshalOptions{Deterministic: true}).Marshal(&commonpb.Payload{Metadata: p.Metadata})
}
func dataCipher(dek []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, ErrInvalidPayload
	}
	return cipher.NewGCM(block)
}
