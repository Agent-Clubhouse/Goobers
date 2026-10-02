package schemas

import "testing"

func TestInstanceSchemaKeyStores(t *testing.T) {
	schema := compileInstanceSchema(t)
	for _, tc := range []struct {
		name, store string
		valid       bool
	}{
		{"file", "{name: local, kind: file-key, directory: /private/keys}", true},
		{"vault", "{name: keys, kind: keyvault-key, vaultURI: 'https://test.vault.azure.net', auth: {kind: managed-identity}}", true},
		{"missing directory", "{name: local, kind: file-key}", false},
		{"file auth", "{name: local, kind: file-key, directory: /private/keys, auth: {kind: azure-cli}}", false},
		{"vault directory", "{name: keys, kind: keyvault-key, directory: /private/keys, vaultURI: 'https://test.vault.azure.net', auth: {kind: azure-cli}}", false},
		{"key cache", "{name: local, kind: file-key, directory: /private/keys, cacheTTLSeconds: 30}", false},
		{"vault missing auth", "{name: keys, kind: keyvault-key, vaultURI: 'https://test.vault.azure.net'}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInstanceYAML(t, schema, "apiVersion: goobers.dev/v1alpha1\nkind: Instance\nrepos: []\nsecretStores:\n - "+tc.store+"\n")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, err=%v", tc.valid, err)
			}
		})
	}
}
