package secretstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"

	"github.com/goobers/goobers/internal/instance"
)

type azureKeyStore struct {
	client   *azkeys.Client
	vaultURI string
}

func newAzureKeyStore(vaultURI string, credential azcore.TokenCredential, options *azkeys.ClientOptions) (KeyStore, error) {
	client, err := azkeys.NewClient(vaultURI, credential, options)
	if err != nil {
		return nil, fmt.Errorf("create Azure Key Vault keys client: %w", err)
	}
	return &azureKeyStore{client: client, vaultURI: strings.TrimSuffix(vaultURI, "/")}, nil
}

func (s *azureKeyStore) Wrap(ctx context.Context, ref instance.KeyRef, plaintext []byte) ([]byte, string, error) {
	if err := validateKeyOperation(ctx, ref, plaintext, false); err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	algorithm := azkeys.EncryptionAlgorithmRSAOAEP256
	response, err := s.client.WrapKey(ctx, ref.Name, ref.Version, azkeys.KeyOperationParameters{Algorithm: &algorithm, Value: plaintext}, nil)
	if err != nil {
		return nil, "", azureKeyOperationError(ctx, "wrap", err)
	}
	version, err := s.resultVersion(ref, response.KeyOperationResult)
	if err != nil {
		return nil, "", err
	}
	return response.Result, version, nil
}

func (s *azureKeyStore) Unwrap(ctx context.Context, ref instance.KeyRef, ciphertext []byte) ([]byte, error) {
	if err := validateKeyOperation(ctx, ref, ciphertext, true); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	algorithm := azkeys.EncryptionAlgorithmRSAOAEP256
	response, err := s.client.UnwrapKey(ctx, ref.Name, ref.Version, azkeys.KeyOperationParameters{Algorithm: &algorithm, Value: ciphertext}, nil)
	if err != nil {
		return nil, azureKeyOperationError(ctx, "unwrap", err)
	}
	if _, err := s.resultVersion(ref, response.KeyOperationResult); err != nil {
		clear(response.Result)
		return nil, err
	}
	return response.Result, nil
}

func (s *azureKeyStore) resultVersion(ref instance.KeyRef, result azkeys.KeyOperationResult) (string, error) {
	if result.KID == nil || len(result.Result) == 0 || len(result.Result) > 1024 {
		return "", fmt.Errorf("azure key operation returned an invalid result")
	}
	raw := string(*result.KID)
	prefix := s.vaultURI + "/keys/" + ref.Name + "/"
	version := strings.TrimPrefix(raw, prefix)
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(raw, prefix) || !instance.ValidKeyComponent(version) {
		return "", fmt.Errorf("azure key operation returned a mismatched key identifier")
	}
	if ref.Version != "" && ref.Version != version {
		return "", fmt.Errorf("azure key operation returned a mismatched key version")
	}
	return version, nil
}

// Do not surface SDK response bodies: a backend may echo operation material.
// Preserve cancellation and status for callers without retaining raw responses.
func azureKeyOperationError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("azure key %s: %w", operation, ctx.Err())
	}
	var response *azcore.ResponseError
	if errors.As(err, &response) {
		return fmt.Errorf("azure key %s failed (HTTP %d)", operation, response.StatusCode)
	}
	return fmt.Errorf("azure key %s failed", operation)
}
