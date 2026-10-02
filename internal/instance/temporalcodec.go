package instance

import "fmt"

// TemporalConfig declares Temporal-specific library configuration. Runtime
// activation is a separate integration step; this declaration alone does not
// change the data converter used by existing Temporal clients.
type TemporalConfig struct {
	PayloadCodec *PayloadCodecConfig `json:"payloadCodec,omitempty" yaml:"payloadCodec,omitempty"`
}

// PayloadCodecConfig selects the wrapping key for sealed Temporal payloads.
// Unset KeyRef preserves the default converter. Strict rejects legacy plaintext
// when the configured codec is explicitly constructed by a caller.
type PayloadCodecConfig struct {
	KeyRef *KeyRef `json:"keyRef,omitempty" yaml:"keyRef,omitempty"`
	Strict bool    `json:"strict,omitempty" yaml:"strict,omitempty"`
}

// TemporalPayloadCodec returns the declared codec settings, or nil.
func (c *Config) TemporalPayloadCodec() *PayloadCodecConfig {
	if c == nil || c.Temporal == nil {
		return nil
	}
	return c.Temporal.PayloadCodec
}

func (c *Config) validateTemporalPayloadCodec() error {
	codec := c.TemporalPayloadCodec()
	if codec == nil {
		return nil
	}
	if codec.KeyRef == nil {
		if codec.Strict {
			return fmt.Errorf("temporal.payloadCodec.strict requires keyRef")
		}
		return nil
	}
	if err := c.ValidateKeyRef(*codec.KeyRef); err != nil {
		return fmt.Errorf("temporal.payloadCodec.keyRef: %w", err)
	}
	return nil
}
