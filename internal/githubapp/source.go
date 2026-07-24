// Package githubapp mints GitHub App installation tokens (#686): short-lived,
// installation-scoped credentials exchanged for a signed App JWT, replacing
// static PATs behind the credentials.Resolver seam. The App private key never
// leaves this process — stages, git subprocesses, and providers only ever see
// minted installation tokens, which GitHub expires after about an hour.
package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
)

const (
	// DefaultBaseURL is the api.github.com endpoint. A GitHub Enterprise
	// deployment overrides it with its API root; tests point it at a fake.
	DefaultBaseURL = "https://api.github.com"

	// appJWTLifetime bounds the App JWT. GitHub rejects JWTs valid for more
	// than 10 minutes; 9 leaves room for the backdate below without ever
	// crossing that cap as measured from iat.
	appJWTLifetime = 9 * time.Minute
	// appJWTBackdate is the clock-drift allowance GitHub's own docs
	// recommend: iat is set slightly in the past so a fast API clock does
	// not reject a just-issued JWT.
	appJWTBackdate = time.Minute

	// refreshSkew re-mints an installation token this long before its
	// stated expiry, so a token handed to a stage or git subprocess is
	// never moments from dying (same margin as providers' ADO bearer cache).
	refreshSkew = 5 * time.Minute

	mintTimeout = 30 * time.Second
)

// SecretRegistrar receives every private key and minted token this package
// resolves, before either is returned or used — the journal scrubber's shape
// (credentials.SecretRegistrar, satisfied structurally).
type SecretRegistrar interface {
	Register(secret []byte)
}

// Config assembles one installation-token minting source.
type Config struct {
	// AppID is the JWT issuer: the App's numeric ID or client ID string.
	AppID string
	// InstallationID is the numeric installation the tokens are minted for.
	InstallationID string
	// Repositories down-scopes every minted token to these repository names
	// (no owner prefix). Empty mints against the installation's full
	// repository set. Source always pins this to the one configured repo,
	// so when several repos share an App installation a token leaked from
	// one gaggle's stage cannot reach a sibling gaggle's repo (MGV-5,
	// #1012). Permission down-scoping per mint is a possible further
	// tightening; the repository list is the isolation boundary that
	// matters across gaggles.
	Repositories []string
	// Key returns the App's PEM-encoded RSA private key. It is re-resolved
	// on every mint, so a rotated key file takes effect without restarting
	// the process (the env/file resolver's contract).
	Key func(ctx context.Context) (string, error)
	// BaseURL overrides DefaultBaseURL (GitHub Enterprise, tests).
	BaseURL string
	// HTTPClient overrides http.DefaultClient.
	HTTPClient *http.Client
	// Registrar receives the private key and every minted token. Nil skips
	// registration — only for preflight paths that never write journals
	// (mirrors adoauth.Provider's nil-registrar preflight use).
	Registrar SecretRegistrar
	// Now overrides time.Now for cache-expiry tests.
	Now func() time.Time
}

// TokenSource mints installation tokens on demand and caches the current one
// until near expiry. Token is safe for concurrent use: the mutex is held
// across a mint, so concurrent callers single-flight onto one exchange and
// then share the cached result.
type TokenSource struct {
	cfg Config

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// New validates cfg and returns a minting TokenSource.
func New(cfg Config) (*TokenSource, error) {
	if cfg.AppID == "" {
		return nil, errors.New("githubapp: app ID is required")
	}
	if cfg.InstallationID == "" {
		return nil, errors.New("githubapp: installation ID is required")
	}
	if cfg.Key == nil {
		return nil, errors.New("githubapp: private key source is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &TokenSource{cfg: cfg}, nil
}

// Source builds the minting token source for one github-app-authenticated
// instance repo (the adoauth.Source counterpart). The private key ref is
// resolved through the same env/file machinery as any token ref, per mint.
func Source(repo instance.RepoRef, registrar SecretRegistrar) (*TokenSource, error) {
	if !repo.GitHubAppAuth() {
		return nil, fmt.Errorf("githubapp: repo %s/%s does not use github-app auth", repo.Owner, repo.Name)
	}
	const refName = "github-app-private-key"
	// Fail closed on a store-backed key ref until store resolution is wired
	// through this path (#683): a store ref must never silently degrade.
	env, file, err := repo.Auth.PrivateKey.EnvFileSources()
	if err != nil {
		return nil, fmt.Errorf("githubapp: configure App key source: %w", err)
	}
	keyResolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: refName, Env: env, File: file}})
	if err != nil {
		return nil, fmt.Errorf("githubapp: configure App key source: %w", err)
	}
	return New(Config{
		AppID:          string(repo.Auth.AppID),
		InstallationID: string(repo.Auth.InstallationID),
		// Down-scope every mint to this one repo: a shared App
		// installation must not hand one gaggle's stages a token that
		// reaches another gaggle's repo (per-repo scoping, MGV-5 #1012).
		Repositories: []string{repo.Name},
		Key: func(ctx context.Context) (string, error) {
			return keyResolver.Resolve(ctx, refName)
		},
		Registrar: registrar,
	})
}

// Token returns a currently-valid installation token, minting one when the
// cache is empty or within refreshSkew of expiry. Mint failures fail closed:
// no stale token is returned and nothing falls back to a static credential.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.cfg.Now()
	if s.token != "" && now.Add(refreshSkew).Before(s.expiresAt) {
		return s.token, nil
	}
	token, expiresAt, err := s.mint(ctx, now)
	if err != nil {
		return "", err
	}
	if s.cfg.Registrar != nil {
		s.cfg.Registrar.Register([]byte(token))
	}
	s.token, s.expiresAt = token, expiresAt
	return token, nil
}

// mint signs a fresh App JWT and exchanges it for an installation token.
func (s *TokenSource) mint(ctx context.Context, now time.Time) (string, time.Time, error) {
	keyPEM, err := s.cfg.Key(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("githubapp: resolve App private key: %w", err)
	}
	if s.cfg.Registrar != nil {
		s.cfg.Registrar.Register([]byte(keyPEM))
	}
	key, err := parseRSAPrivateKey(keyPEM)
	if err != nil {
		return "", time.Time{}, err
	}
	claims := jwt.RegisteredClaims{
		Issuer:    s.cfg.AppID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-appJWTBackdate)),
		ExpiresAt: jwt.NewNumericDate(now.Add(appJWTLifetime)),
	}
	appJWT, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("githubapp: sign App JWT: %w", err)
	}

	mintCtx, cancel := context.WithTimeout(ctx, mintTimeout)
	defer cancel()
	endpoint := strings.TrimRight(s.cfg.BaseURL, "/") + "/app/installations/" + url.PathEscape(s.cfg.InstallationID) + "/access_tokens"
	var reqBody io.Reader
	if len(s.cfg.Repositories) > 0 {
		payload, err := json.Marshal(struct {
			Repositories []string `json:"repositories"`
		}{s.cfg.Repositories})
		if err != nil {
			return "", time.Time{}, fmt.Errorf("githubapp: encode token request: %w", err)
		}
		reqBody = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(mintCtx, http.MethodPost, endpoint, reqBody)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("githubapp: build token request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+appJWT)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("githubapp: exchange App JWT for installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("githubapp: read token response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, mintError(resp.StatusCode, body, s.cfg.AppID, s.cfg.InstallationID)
	}

	var payload struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", time.Time{}, fmt.Errorf("githubapp: decode installation token response: %w", err)
	}
	if strings.TrimSpace(payload.Token) == "" {
		return "", time.Time{}, errors.New("githubapp: installation token response is missing token")
	}
	if payload.ExpiresAt.IsZero() || !payload.ExpiresAt.After(now) {
		return "", time.Time{}, errors.New("githubapp: installation token response has an invalid expiry")
	}
	return payload.Token, payload.ExpiresAt, nil
}

// mintError maps GitHub's failure statuses onto actionable diagnostics. Only
// the API's own error message is quoted — never the JWT, key, or a token.
func mintError(status int, body []byte, appID, installationID string) error {
	message := githubErrorMessage(body)
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("githubapp: GitHub rejected the App JWT for app %s: %s — check that auth.appId matches the App and auth.privateKey is a current key for it (a revoked or regenerated key signs JWTs GitHub refuses)", appID, message)
	case http.StatusNotFound:
		return fmt.Errorf("githubapp: installation %s not found for app %s: %s — install the App on the repository's owner and use that installation's ID", installationID, appID, message)
	case http.StatusForbidden:
		return fmt.Errorf("githubapp: GitHub refused to mint an installation token for app %s installation %s: %s — check the App's granted permissions and that the installation is not suspended", appID, installationID, message)
	default:
		return fmt.Errorf("githubapp: mint installation token for app %s installation %s: status %d: %s", appID, installationID, status, message)
	}
}

// githubErrorMessage extracts the "message" field GitHub error bodies carry,
// falling back to a fixed placeholder — the raw body is never echoed, so a
// malformed or proxy-mangled response cannot smuggle secrets into an error.
func githubErrorMessage(body []byte) string {
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || strings.TrimSpace(payload.Message) == "" {
		return "(no error message)"
	}
	message := strings.TrimSpace(payload.Message)
	const maxMessage = 200
	if len(message) > maxMessage {
		message = message[:maxMessage] + "…"
	}
	return message
}

// parseRSAPrivateKey decodes a PEM-encoded RSA private key in either the
// PKCS#1 form GitHub App keys download as or the generic PKCS#8 form a
// re-encoded key may carry. Error messages never include key material.
func parseRSAPrivateKey(keyPEM string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return nil, errors.New("githubapp: App private key is not PEM-encoded")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("githubapp: App private key is not a valid PKCS#1 or PKCS#8 RSA key")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("githubapp: App private key is not an RSA key (GitHub App keys are RSA)")
	}
	return key, nil
}
