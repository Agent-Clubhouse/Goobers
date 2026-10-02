// Package temporalcodec provides an opt-in Temporal payload codec and its
// authenticated remote HTTP contract. It has no runtime registration or side
// effects until a caller explicitly constructs and uses it.
package temporalcodec

import (
	"fmt"

	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/secretstore"
)

// DataConverter returns the exact SDK default when keyRef is unset. Otherwise
// it constructs the sealed codec, without performing a key operation or dialing
// Temporal. Callers must explicitly attach the result to client options.
func DataConverter(cfg *instance.Config) (converter.DataConverter, error) {
	settings := cfg.TemporalPayloadCodec()
	if settings == nil || settings.KeyRef == nil {
		if settings != nil && settings.Strict {
			return nil, fmt.Errorf("payload codec strict mode requires keyRef")
		}
		return converter.GetDefaultDataConverter(), nil
	}
	codec, err := FromConfig(cfg)
	if err != nil {
		return nil, err
	}
	return converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), codec), nil
}

// FromConfig constructs the configured codec for explicit library or HTTP use.
// It rejects an absent key instead of silently creating a plaintext endpoint.
func FromConfig(cfg *instance.Config) (*Codec, error) {
	settings := cfg.TemporalPayloadCodec()
	if settings == nil || settings.KeyRef == nil {
		return nil, fmt.Errorf("payload codec requires keyRef")
	}
	if err := cfg.ValidateKeyRef(*settings.KeyRef); err != nil {
		return nil, err
	}
	// Construct only the selected key store. Unused stores do not authenticate.
	for _, store := range cfg.SecretStores {
		if store.Name != settings.KeyRef.Store {
			continue
		}
		registry, err := secretstore.NewKeyRegistry([]instance.SecretStoreConfig{store})
		if err != nil {
			return nil, err
		}
		return New(registry, *settings.KeyRef, settings.Strict)
	}
	return nil, fmt.Errorf("payload codec key store is not declared")
}
