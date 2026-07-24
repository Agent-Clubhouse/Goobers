package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	// AzureDevOpsResourceID is the Microsoft Entra resource identifier for
	// Azure DevOps.
	AzureDevOpsResourceID = "499b84ac-1321-427f-aa17-267ca6975798"

	adoCredentialPAT    = "pat"
	adoCredentialBearer = "bearer"

	adoTokenRefreshSkew = 5 * time.Minute
)

// ADOCredential is one authorization value returned by an ADOCredentialSource.
// Secret is the raw PAT or bearer token, never a complete Authorization header.
type ADOCredential struct {
	Kind      string
	Secret    string
	Username  string
	ExpiresAt time.Time
}

// ADOCredentialSource resolves credentials for Azure DevOps requests. Sources
// may cache expiring tokens, but must honor context cancellation.
type ADOCredentialSource interface {
	Credential(context.Context) (ADOCredential, error)
}

type refreshableADOCredentialSource interface {
	ADOCredentialSource
	Invalidate()
}

type adoPATCredentialSource struct {
	username string
	resolve  func(context.Context) (string, error)
}

// NewADOPATCredentialSource returns a static PAT source. The provider preserves
// the historical "goobers" username when username is empty.
func NewADOPATCredentialSource(username, token string) ADOCredentialSource {
	return NewResolvingADOPATCredentialSource(username, func(context.Context) (string, error) {
		return token, nil
	})
}

// NewResolvingADOPATCredentialSource returns a PAT source that resolves its
// value for every operation, allowing env/file-backed rotation.
func NewResolvingADOPATCredentialSource(username string, resolve func(context.Context) (string, error)) ADOCredentialSource {
	if username == "" {
		username = "goobers"
	}
	return &adoPATCredentialSource{username: username, resolve: resolve}
}

func (s *adoPATCredentialSource) Credential(ctx context.Context) (ADOCredential, error) {
	if err := ctx.Err(); err != nil {
		return ADOCredential{}, err
	}
	if s.resolve == nil {
		return ADOCredential{}, fmt.Errorf("ado PAT credential resolver is nil")
	}
	token, err := s.resolve(ctx)
	if err != nil {
		return ADOCredential{}, err
	}
	if strings.TrimSpace(token) == "" {
		return ADOCredential{}, fmt.Errorf("ado PAT credential is empty")
	}
	return ADOCredential{Kind: adoCredentialPAT, Secret: token, Username: s.username}, nil
}

func (c ADOCredential) authorizationHeader() (string, error) {
	switch c.Kind {
	case adoCredentialPAT:
		if strings.TrimSpace(c.Secret) == "" {
			return "", fmt.Errorf("ado PAT credential is empty")
		}
		username := c.Username
		if username == "" {
			username = "goobers"
		}
		return basicAuth(username, c.Secret), nil
	case adoCredentialBearer:
		if strings.TrimSpace(c.Secret) == "" {
			return "", fmt.Errorf("ado bearer credential is empty")
		}
		return "Bearer " + c.Secret, nil
	default:
		return "", fmt.Errorf("unsupported ado credential kind %q", c.Kind)
	}
}

func adoGitAuthEnv(header, remoteURL string) []string {
	scopedURL := strings.TrimRight(remoteURL, "/") + "/"
	base := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if upper == "GIT_CONFIG_COUNT" || upper == "GIT_TERMINAL_PROMPT" ||
			strings.HasPrefix(upper, "GIT_CONFIG_KEY_") || strings.HasPrefix(upper, "GIT_CONFIG_VALUE_") {
			continue
		}
		base = append(base, entry)
	}
	return append(base,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=http."+scopedURL+".extraheader",
		"GIT_CONFIG_VALUE_1=AUTHORIZATION: "+header,
		"GIT_TERMINAL_PROMPT=0",
	)
}

// ADOGitAuthEnvironment resolves one credential into a child-process-only Git
// environment. The returned environment must never be persisted or journaled.
func ADOGitAuthEnvironment(ctx context.Context, source ADOCredentialSource, registrar SecretRegistrar, remoteURL string) ([]string, error) {
	if source == nil {
		return nil, fmt.Errorf("ADO Git authentication requires a credential source")
	}
	if strings.TrimSpace(remoteURL) == "" {
		return nil, fmt.Errorf("ADO Git authentication requires a remote URL")
	}
	credential, err := source.Credential(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve ADO Git credential: %w", err)
	}
	header, err := credential.authorizationHeader()
	if err != nil {
		return nil, err
	}
	if registrar != nil {
		registrar.Register([]byte(credential.Secret))
		registrar.Register([]byte(strings.TrimSpace(strings.TrimPrefix(header, "Basic "))))
	}
	return adoGitAuthEnv(header, remoteURL), nil
}

type adoBearerToken struct {
	token     string
	expiresAt time.Time
}

type cachedADOBearerSource struct {
	mu    sync.Mutex
	now   func() time.Time
	fetch func(context.Context) (adoBearerToken, error)

	token adoBearerToken
}

func newCachedADOBearerSource(now func() time.Time, fetch func(context.Context) (adoBearerToken, error)) *cachedADOBearerSource {
	if now == nil {
		now = time.Now
	}
	return &cachedADOBearerSource{now: now, fetch: fetch}
}

func (s *cachedADOBearerSource) Credential(ctx context.Context) (ADOCredential, error) {
	if err := ctx.Err(); err != nil {
		return ADOCredential{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if s.token.token != "" && now.Add(adoTokenRefreshSkew).Before(s.token.expiresAt) {
		return ADOCredential{Kind: adoCredentialBearer, Secret: s.token.token, ExpiresAt: s.token.expiresAt}, nil
	}
	token, err := s.fetch(ctx)
	if err != nil {
		return ADOCredential{}, err
	}
	if strings.TrimSpace(token.token) == "" {
		return ADOCredential{}, fmt.Errorf("ado bearer credential source returned an empty token")
	}
	if token.expiresAt.IsZero() || !token.expiresAt.After(now) {
		return ADOCredential{}, fmt.Errorf("ado bearer credential source returned an invalid expiry")
	}
	s.token = token
	return ADOCredential{Kind: adoCredentialBearer, Secret: token.token, ExpiresAt: token.expiresAt}, nil
}

func (s *cachedADOBearerSource) Invalidate() {
	s.mu.Lock()
	s.token = adoBearerToken{}
	s.mu.Unlock()
}

// NewAzureCLIADOCredentialSource reuses the current Azure CLI login. tenant is
// optional; when set, az requests the token from that tenant explicitly.
func NewAzureCLIADOCredentialSource(runner CommandRunner, tenant string) ADOCredentialSource {
	runner = commandRunnerOrDefault(runner)
	return newCachedADOBearerSource(time.Now, func(ctx context.Context) (adoBearerToken, error) {
		args := []string{
			"account", "get-access-token",
			"--resource", AzureDevOpsResourceID,
			"--output", "json",
		}
		if tenant != "" {
			args = append(args, "--tenant", tenant)
		}
		out, err := runner.Run(ctx, "az", args...)
		if err != nil {
			// Azure CLI output can contain credential material on partial
			// failures, so never copy it into the returned error.
			return adoBearerToken{}, fmt.Errorf("azure CLI get-access-token: %w", err)
		}
		return parseAzureCLIAccessToken(out)
	})
}

type azureTokenCredential interface {
	GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error)
}

func newAzureIdentityADOCredentialSource(credential azureTokenCredential) ADOCredentialSource {
	return newCachedADOBearerSource(time.Now, func(ctx context.Context) (adoBearerToken, error) {
		token, err := credential.GetToken(ctx, policy.TokenRequestOptions{
			Scopes: []string{AzureDevOpsResourceID + "/.default"},
		})
		if err != nil {
			return adoBearerToken{}, fmt.Errorf("azure identity get Azure DevOps token: %w", err)
		}
		return adoBearerToken{token: token.Token, expiresAt: token.ExpiresOn}, nil
	})
}

// NewWorkloadIdentityADOCredentialSource uses Azure workload identity
// federation configured through the standard AZURE_* environment variables.
func NewWorkloadIdentityADOCredentialSource() (ADOCredentialSource, error) {
	credential, err := azidentity.NewWorkloadIdentityCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure workload identity credential: %w", err)
	}
	return newAzureIdentityADOCredentialSource(credential), nil
}

// NewManagedIdentityADOCredentialSource uses an Azure managed identity.
// clientID selects a user-assigned identity; empty uses the system identity.
func NewManagedIdentityADOCredentialSource(clientID string) (ADOCredentialSource, error) {
	options := &azidentity.ManagedIdentityCredentialOptions{}
	if clientID != "" {
		options.ID = azidentity.ClientID(clientID)
	}
	credential, err := azidentity.NewManagedIdentityCredential(options)
	if err != nil {
		return nil, fmt.Errorf("create Azure managed identity credential: %w", err)
	}
	return newAzureIdentityADOCredentialSource(credential), nil
}

func parseAzureCLIAccessToken(data []byte) (adoBearerToken, error) {
	var payload struct {
		AccessToken string          `json:"accessToken"`
		ExpiresOn   json.RawMessage `json:"expiresOn"`
		ExpiresUnix json.RawMessage `json:"expires_on"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return adoBearerToken{}, fmt.Errorf("decode Azure CLI access token response: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return adoBearerToken{}, fmt.Errorf("azure CLI access token response is missing accessToken")
	}
	expiresAt, err := parseAzureCLIExpiry(payload.ExpiresUnix)
	if err != nil || expiresAt.IsZero() {
		expiresAt, err = parseAzureCLIExpiry(payload.ExpiresOn)
	}
	if err != nil || expiresAt.IsZero() {
		return adoBearerToken{}, fmt.Errorf("azure CLI access token response has an invalid expiry")
	}
	return adoBearerToken{token: payload.AccessToken, expiresAt: expiresAt}, nil
}

func parseAzureCLIExpiry(raw json.RawMessage) (time.Time, error) {
	value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if value == "" || value == "null" {
		return time.Time{}, fmt.Errorf("expiry is empty")
	}
	if unix, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Unix(unix, 0), nil
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999 -07:00",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999",
		time.DateTime,
	} {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized expiry format")
}
