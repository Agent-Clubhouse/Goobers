package enginestartintent

import (
	"encoding/json"
	"errors"
	"path/filepath"

	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/temporalcodec"
	"github.com/goobers/goobers/internal/temporaldial"
)

// CredentialBinding hashes the configured transport and selected codec store.
// Nothing from this object is persisted except the digest; materialization still
// uses the current matching configuration and normal secret-store resolver.
func CredentialBinding(cfg *instance.Config) (string, error) {
	if cfg == nil {
		return "", errors.New("direct engine: instance credentials unavailable")
	}
	var selected *instance.SecretStoreConfig
	settings := cfg.TemporalPayloadCodec()
	if settings != nil && settings.KeyRef != nil {
		for i := range cfg.SecretStores {
			if cfg.SecretStores[i].Name == settings.KeyRef.Store {
				selected = &cfg.SecretStores[i]
				break
			}
		}
		if selected == nil {
			return "", errors.New("direct engine: configured codec store unavailable")
		}
	}
	raw, err := json.Marshal(struct {
		TLS   *temporaldial.TLS
		Codec *instance.PayloadCodecConfig
		Store *instance.SecretStoreConfig
	}{cfg.EffectiveEngineConfig().TLS, settings, selected})
	if err != nil {
		return "", err
	}
	return Digest(raw), nil
}

// Transport resolves only the currently matching credential selectors. Relative
// filesystem selectors retain the accepting CLI's base without a process chdir.
func Transport(cfg *instance.Config, request Request) (*temporaldial.TLS, converter.DataConverter, error) {
	binding, err := CredentialBinding(cfg)
	if err != nil {
		return nil, nil, err
	}
	if binding != request.Binding {
		return nil, nil, errors.New("direct engine: configured transport or codec binding changed")
	}
	var tls *temporaldial.TLS
	if original := cfg.EffectiveEngineConfig().TLS; original != nil {
		copyTLS := *original
		copyTLS.CAFile = absoluteSelector(request.Directory, copyTLS.CAFile)
		copyTLS.CertFile = absoluteSelector(request.Directory, copyTLS.CertFile)
		copyTLS.KeyFile = absoluteSelector(request.Directory, copyTLS.KeyFile)
		tls = &copyTLS
	}
	copyConfig := *cfg
	copyConfig.SecretStores = append([]instance.SecretStoreConfig(nil), cfg.SecretStores...)
	for i := range copyConfig.SecretStores {
		copyConfig.SecretStores[i].Directory = absoluteSelector(request.Directory, copyConfig.SecretStores[i].Directory)
	}
	dc, err := temporalcodec.DataConverter(&copyConfig)
	return tls, dc, err
}

func absoluteSelector(directory, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(directory, path)
}
