package decider

import (
	"context"
	"errors"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// EntraScope is the token audience for Azure AI Foundry model endpoints.
const EntraScope = "https://cognitiveservices.azure.com/.default"

// NewEntraTokenSource returns a TokenSource backed by the ambient Azure
// identity chain (workload identity, managed identity, then developer
// sign-in), so no key is stored anywhere. The credential caches tokens and
// refreshes them before expiry.
func NewEntraTokenSource() (func(context.Context) (string, error), error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, errors.New("decider: no Azure identity available")
	}
	return tokenSourceFrom(cred), nil
}

func tokenSourceFrom(cred azcore.TokenCredential) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		tok, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{EntraScope}})
		if err != nil {
			return "", errors.New("decider: token acquisition failed")
		}
		return tok.Token, nil
	}
}
