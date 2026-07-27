package secretstore

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"

	"github.com/goobers/goobers/internal/instance"
)

// newAzureKeyVaultStore builds the azure-key-vault Store for one declared
// store: an azsecrets client authenticated by exactly the configured ambient
// identity. There is deliberately no DefaultAzureCredential chain — the auth
// kind the instance declares is the only one tried, mirroring
// internal/adoauth's explicit-kind selection, so a misconfigured identity is
// a diagnosable error instead of a silent fallback to whichever ambient
// credential happens to work.
func newAzureKeyVaultStore(cfg instance.SecretStoreConfig) (Store, error) {
	if cfg.Kind != instance.SecretStoreKindAzureKeyVault {
		return nil, fmt.Errorf("unsupported secret store kind %q (supported: %q)", cfg.Kind, instance.SecretStoreKindAzureKeyVault)
	}
	credential, err := azureStoreCredential(cfg)
	if err != nil {
		return nil, err
	}
	return newAzureKeyVaultStoreWithCredential(cfg.VaultURI, credential, nil)
}

// azureStoreCredential maps the store's declared auth kind onto the matching
// azidentity credential. Validation restricts the kinds at config load; the
// default arm keeps this fail-closed if a registry is ever built from an
// unvalidated config.
func azureStoreCredential(cfg instance.SecretStoreConfig) (azcore.TokenCredential, error) {
	if cfg.Auth == nil {
		return nil, fmt.Errorf("auth is required for kind %q", instance.SecretStoreKindAzureKeyVault)
	}
	switch cfg.Auth.Kind {
	case instance.SecretStoreAuthWorkloadIdentity:
		options := &azidentity.WorkloadIdentityCredentialOptions{}
		if cfg.Auth.ClientID != "" {
			options.ClientID = cfg.Auth.ClientID
		}
		credential, err := azidentity.NewWorkloadIdentityCredential(options)
		if err != nil {
			return nil, fmt.Errorf("create Azure workload identity credential: %w", err)
		}
		return credential, nil
	case instance.SecretStoreAuthManagedIdentity:
		options := &azidentity.ManagedIdentityCredentialOptions{}
		if cfg.Auth.ClientID != "" {
			options.ID = azidentity.ClientID(cfg.Auth.ClientID)
		}
		credential, err := azidentity.NewManagedIdentityCredential(options)
		if err != nil {
			return nil, fmt.Errorf("create Azure managed identity credential: %w", err)
		}
		return credential, nil
	case instance.SecretStoreAuthAzureCLI:
		credential, err := azidentity.NewAzureCLICredential(nil)
		if err != nil {
			return nil, fmt.Errorf("create Azure CLI credential: %w", err)
		}
		return credential, nil
	default:
		return nil, fmt.Errorf("unsupported secret store auth kind %q (supported: %q, %q, %q)",
			cfg.Auth.Kind, instance.SecretStoreAuthWorkloadIdentity, instance.SecretStoreAuthManagedIdentity, instance.SecretStoreAuthAzureCLI)
	}
}

// newAzureKeyVaultStoreWithCredential finishes construction from an already
// bootstrapped credential; tests inject a fake credential and transport here.
func newAzureKeyVaultStoreWithCredential(vaultURI string, credential azcore.TokenCredential, options *azsecrets.ClientOptions) (Store, error) {
	client, err := azsecrets.NewClient(vaultURI, credential, options)
	if err != nil {
		return nil, fmt.Errorf("create Azure Key Vault client for %s: %w", vaultURI, err)
	}
	return &azureKeyVaultStore{client: client}, nil
}

type azureKeyVaultStore struct {
	client *azsecrets.Client
}

// FetchSecret reads the latest version of the named secret. Secret names are
// vault-relative (no version pin); rotation happens in the vault and the
// cache TTL bounds how stale a resolved value can be.
func (s *azureKeyVaultStore) FetchSecret(ctx context.Context, name string) (string, error) {
	response, err := s.client.GetSecret(ctx, name, "", nil)
	if err != nil {
		return "", fmt.Errorf("get secret: %w", err)
	}
	if response.Value == nil {
		return "", fmt.Errorf("get secret: secret has no value")
	}
	return *response.Value, nil
}
