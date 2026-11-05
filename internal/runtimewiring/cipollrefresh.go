package runtimewiring

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// podCIPollADOCredentialSource keeps scheme, value and expiry from the same
// broker response. It never reads ambient stage credentials or instance config.

// MintedADOCredential carries an ADO token together with its expiry.
func MintedADOCredential(creds []dispatcher.MintedCredential, fallback, name string) (providers.ADOCredential, error) {
	var out providers.ADOCredential
	count := 0
	for _, cred := range creds {
		if cred.Capability != name {
			continue
		}
		count++
		switch strings.ToLower(strings.TrimSpace(cred.SchemeOr(fallback))) {
		case "basic":
			out.Kind = providers.ADOCredentialKindPAT
		case "bearer":
			out.Kind = providers.ADOCredentialKindBearer
		default:
			return out, fmt.Errorf("%w for %s: explicit Basic or Bearer scheme required", providers.ErrInvalidADOCredentialDelivery, name)
		}
		out.Secret = cred.Value
		if cred.ExpiresAt != nil {
			out.ExpiresAt = *cred.ExpiresAt
		}
	}
	if count != 1 || strings.TrimSpace(out.Secret) == "" {
		return out, fmt.Errorf("%w for %s: expected exactly one nonempty credential", providers.ErrInvalidADOCredentialDelivery, name)
	}
	return out, nil
}

// CiPollTokenSource resolves capabilityName through injector and wraps the
// value in a refreshing source (newCIPollRefreshingToken): refreshed ahead of
// a stated expiry, and re-resolved after a 401 whether or not it states one.
func CiPollTokenSource(ctx context.Context, injector *credentials.Injector, capabilityName string, registrar credentials.SecretRegistrar) (string, providers.TokenSource, error) {
	token, expiresAt, err := materializeCapability(ctx, injector, capabilityName)
	if err != nil {
		return "", nil, err
	}
	refresh := func(ctx context.Context) (string, time.Time, error) {
		return materializeCapability(ctx, injector, capabilityName)
	}
	source, err := NewCIPollRefreshingToken(capabilityName, token, expiresAt, refresh, registrar)
	if err != nil {
		return "", nil, err
	}
	return token, source, nil
}

// materializeCapability resolves one capability through injector: a fresh
// value per call (the injector's resolver re-mints a GitHub App token near
// its expiry), registered with the injector's scrubber registrar.
func materializeCapability(ctx context.Context, injector *credentials.Injector, capabilityName string) (string, time.Time, error) {
	set, err := injector.Materialize(ctx, []string{capabilityName})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("resolve ci-poll credentials: %w", err)
	}
	token, err := set.Token(ctx, capabilityName)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("resolve ci-poll credential: %w", err)
	}
	expiresAt, _ := set.Expiry(capabilityName)
	return token, expiresAt, nil
}

// LocalCIPollGitHubPoller is the local ci-poll's GitHub poller: through the
// newPRPoller test seam when set, else a provider that resolves its token per
// request from the injector-backed refreshing source (when the value expires)
// and re-resolves it once on 401.
func LocalCIPollGitHubPoller(ctx context.Context, injector *credentials.Injector, capabilityName string, registrar credentials.SecretRegistrar) (executor.PRPoller, error) {
	token, source, err := CiPollTokenSource(ctx, injector, capabilityName, registrar)
	if err != nil {
		return nil, err
	}
	if NewPRPoller != nil {
		return NewPRPoller(token), nil
	}
	if source == nil {
		return providers.NewGitHubProvider(token), nil
	}
	return providers.NewGitHubProvider(token, providers.WithTokenSource(source)), nil
}

// podCIPollTokenSource wraps the value the pod resolved at stage start in a
// refreshing source that re-resolves through the credential plane under the
// pod's own token — the same call that produced it — ahead of a stated expiry
// and after a 401 (newCIPollRefreshingToken). Nil when no value was resolved.

// NewCIPollRefreshingToken is ci-poll's token source. A value with a stated
// expiry is refreshed ahead of it and once after a 401 (Goobers#6120). A value
// without one (a PAT) is never refreshed proactively, but is still re-resolved
// after a 401 — once per ci-poll unauthorized retry (#6154), whose bound
// limits the attempts — rather than on every poll request.
func NewCIPollRefreshingToken(capabilityName, token string, expiresAt time.Time, refresh credentials.RefreshFunc, registrar credentials.SecretRegistrar) (*credentials.RefreshingToken, error) {
	if expiresAt.IsZero() {
		return credentials.NewInvalidationRefreshingToken(capabilityName, token, refresh, registrar)
	}
	return credentials.NewRefreshingToken(capabilityName, token, expiresAt, refresh, registrar)
}

// MintedValueAndExpiry finds a minted capability value and its expiry.
func MintedValueAndExpiry(creds []dispatcher.MintedCredential, capabilityName string) (string, time.Time, error) {
	for _, cred := range creds {
		if cred.Capability != capabilityName {
			continue
		}
		var expiresAt time.Time
		if cred.ExpiresAt != nil {
			expiresAt = *cred.ExpiresAt
		}
		return cred.Value, expiresAt, nil
	}
	return "", time.Time{}, fmt.Errorf("the credential plane returned no value for %q", capabilityName)
}
