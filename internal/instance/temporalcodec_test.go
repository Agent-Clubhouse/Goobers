package instance

import "testing"

func TestTemporalPayloadCodecConfig(t *testing.T) {
	key := SecretStoreConfig{Name: "keys", Kind: SecretStoreKindFileKey, Directory: "/private/keys"}
	for _, tc := range []struct {
		name     string
		settings *PayloadCodecConfig
		store    SecretStoreConfig
		valid    bool
	}{
		{"absent", nil, key, true},
		{"empty", &PayloadCodecConfig{}, key, true},
		{"strict without key", &PayloadCodecConfig{Strict: true}, key, false},
		{"key", &PayloadCodecConfig{KeyRef: &KeyRef{Store: "keys", Name: "history"}}, key, true},
		{"strict", &PayloadCodecConfig{KeyRef: &KeyRef{Store: "keys", Name: "history"}, Strict: true}, key, true},
		{"undeclared", &PayloadCodecConfig{KeyRef: &KeyRef{Store: "missing", Name: "history"}}, key, false},
		{"secret store", &PayloadCodecConfig{KeyRef: &KeyRef{Store: "prod-kv", Name: "history"}}, validSecretStore(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{SecretStores: []SecretStoreConfig{tc.store}, Temporal: &TemporalConfig{PayloadCodec: tc.settings}}
			if err := cfg.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
