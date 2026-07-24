// Package oidcauth implements the httpapi Authenticator seam with generic
// OIDC bearer-token validation (#644, SEC-043). It validates JWT signature,
// expiry, issuer, and audience against one configured issuer's published
// JWKS, then maps a configurable roles claim onto the instance roles
// (view/operate/admin). Vendor-neutral by construction: Entra ID is a
// configured issuer, never a code path.
package oidcauth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/goobers/goobers/internal/httpapi"
)

// signingAlgorithms is the fixed allowlist handed to the JWT parser. RSA
// only: it matches the JWKS key material this package parses, and pinning
// the list defeats algorithm-confusion downgrades (alg=none, HMAC signed
// with the public key).
var signingAlgorithms = []string{"RS256", "RS384", "RS512"}

const (
	// expiryLeeway absorbs small clock skew between issuer and daemon when
	// validating exp/nbf/iat.
	expiryLeeway = 30 * time.Second
	// defaultRefreshInterval rate-limits JWKS refetches triggered by tokens
	// carrying an unknown key ID, so a stream of garbage kids cannot turn the
	// daemon into an issuer-hammering loop. Key rotation still converges: the
	// first token signed by a fresh key triggers one refetch.
	defaultRefreshInterval = time.Minute
	// fetchLimit bounds discovery and JWKS response bodies.
	fetchLimit = 1 << 20
)

// RoleMapping maps issuer claim values onto the three instance roles. A
// claim value absent from every list grants nothing (deny by default).
type RoleMapping struct {
	View    []string
	Operate []string
	Admin   []string
}

// Config selects and constrains the trusted issuer.
type Config struct {
	// Issuer is the OIDC issuer URL, exactly as tokens state it in iss.
	// Discovery reads <issuer>/.well-known/openid-configuration.
	Issuer string
	// Audience is the aud claim value tokens must carry.
	Audience string
	// RolesClaim names the claim carrying role values (e.g. "roles",
	// "groups"). Empty defaults to "roles".
	RolesClaim string
	// Roles maps claim values onto instance roles.
	Roles RoleMapping
	// HTTPClient overrides the discovery/JWKS client. Nil uses a bounded
	// default; tests point it at a local fake issuer.
	HTTPClient *http.Client
}

// Authenticator validates bearer tokens for one issuer. It implements
// httpapi.Authenticator and is safe for concurrent use.
type Authenticator struct {
	issuer     string
	audience   string
	rolesClaim string
	roles      map[string][]httpapi.Role
	client     *http.Client
	parser     *jwt.Parser

	mu              sync.Mutex
	jwksURI         string
	keys            map[string]*rsa.PublicKey
	lastRefresh     time.Time
	refreshInterval time.Duration
}

// New constructs an Authenticator. The issuer is not contacted here: JWKS
// discovery is lazy so daemon startup does not depend on issuer
// availability — until discovery succeeds every request fails closed.
func New(cfg Config) (*Authenticator, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("OIDC issuer is required")
	}
	issuer, err := url.Parse(cfg.Issuer)
	if err != nil || !issuer.IsAbs() || issuer.Host == "" {
		return nil, fmt.Errorf("OIDC issuer %q must be an absolute http(s) URL", cfg.Issuer)
	}
	if scheme := strings.ToLower(issuer.Scheme); scheme != "https" && scheme != "http" {
		return nil, fmt.Errorf("OIDC issuer %q must be an absolute http(s) URL", cfg.Issuer)
	}
	if cfg.Audience == "" {
		return nil, errors.New("OIDC audience is required")
	}
	rolesClaim := cfg.RolesClaim
	if rolesClaim == "" {
		rolesClaim = "roles"
	}
	roles := make(map[string][]httpapi.Role)
	for _, group := range []struct {
		role   httpapi.Role
		values []string
	}{
		{role: httpapi.RoleView, values: cfg.Roles.View},
		{role: httpapi.RoleOperate, values: cfg.Roles.Operate},
		{role: httpapi.RoleAdmin, values: cfg.Roles.Admin},
	} {
		for _, value := range group.values {
			if value == "" {
				return nil, errors.New("OIDC role mapping must not contain empty claim values")
			}
			roles[value] = append(roles[value], group.role)
		}
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Authenticator{
		issuer:     cfg.Issuer,
		audience:   cfg.Audience,
		rolesClaim: rolesClaim,
		roles:      roles,
		client:     client,
		parser: jwt.NewParser(
			jwt.WithValidMethods(signingAlgorithms),
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithAudience(cfg.Audience),
			jwt.WithExpirationRequired(),
			jwt.WithLeeway(expiryLeeway),
		),
		keys:            make(map[string]*rsa.PublicKey),
		refreshInterval: defaultRefreshInterval,
	}, nil
}

// Authenticate validates the request's bearer token and returns the mapped
// principal. Every failure is a plain error — the router answers 401 with a
// generic message, and no token material ever reaches an error string.
func (a *Authenticator) Authenticate(request *http.Request) (*httpapi.Principal, error) {
	token := bearerToken(request)
	if token == "" {
		return nil, errors.New("request carries no bearer token")
	}
	claims := jwt.MapClaims{}
	if _, err := a.parser.ParseWithClaims(token, claims, a.keyFunc(request.Context())); err != nil {
		return nil, fmt.Errorf("validate bearer token: %w", err)
	}
	subject, _ := claims["sub"].(string)
	if subject == "" {
		return nil, errors.New("token carries no subject")
	}
	return &httpapi.Principal{
		Subject: subject,
		Issuer:  a.issuer,
		Name:    displayName(claims),
		Roles:   a.mapRoles(claims),
	}, nil
}

func bearerToken(request *http.Request) string {
	authorization := request.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(authorization) <= len(prefix) || !strings.EqualFold(authorization[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(authorization[len(prefix):])
}

func displayName(claims jwt.MapClaims) string {
	for _, claim := range []string{"preferred_username", "name"} {
		if value, ok := claims[claim].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func (a *Authenticator) mapRoles(claims jwt.MapClaims) []httpapi.Role {
	raw, ok := claims[a.rolesClaim]
	if !ok {
		return nil
	}
	var values []string
	switch claim := raw.(type) {
	case string:
		values = []string{claim}
	case []any:
		for _, item := range claim {
			if value, ok := item.(string); ok {
				values = append(values, value)
			}
		}
	}
	var granted []httpapi.Role
	seen := make(map[httpapi.Role]bool)
	for _, value := range values {
		for _, role := range a.roles[value] {
			if !seen[role] {
				seen[role] = true
				granted = append(granted, role)
			}
		}
	}
	return granted
}

func (a *Authenticator) keyFunc(ctx context.Context) jwt.Keyfunc {
	return func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token header carries no key ID")
		}
		return a.signingKey(ctx, kid)
	}
}

// signingKey resolves kid against the cached JWKS, refetching at most once
// per refreshInterval when the kid is unknown (key rotation).
func (a *Authenticator) signingKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if key, ok := a.keys[kid]; ok {
		return key, nil
	}
	if !a.lastRefresh.IsZero() && time.Since(a.lastRefresh) < a.refreshInterval {
		return nil, fmt.Errorf("issuer JWKS has no signing key %q", kid)
	}
	if err := a.refreshKeysLocked(ctx); err != nil {
		return nil, err
	}
	if key, ok := a.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("issuer JWKS has no signing key %q", kid)
}

type discoveryDocument struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

type jwksDocument struct {
	Keys []jwksKey `json:"keys"`
}

type jwksKey struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (a *Authenticator) refreshKeysLocked(ctx context.Context) error {
	if a.jwksURI == "" {
		discovery := strings.TrimSuffix(a.issuer, "/") + "/.well-known/openid-configuration"
		var document discoveryDocument
		if err := a.fetchJSON(ctx, discovery, &document); err != nil {
			return fmt.Errorf("OIDC discovery: %w", err)
		}
		if document.Issuer != a.issuer {
			return fmt.Errorf("OIDC discovery document declares issuer %q, want %q", document.Issuer, a.issuer)
		}
		if document.JWKSURI == "" {
			return errors.New("OIDC discovery document carries no jwks_uri")
		}
		a.jwksURI = document.JWKSURI
	}
	var document jwksDocument
	if err := a.fetchJSON(ctx, a.jwksURI, &document); err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(document.Keys))
	for _, key := range document.Keys {
		if key.Kty != "RSA" || key.Kid == "" || (key.Use != "" && key.Use != "sig") {
			continue
		}
		public, err := rsaPublicKey(key)
		if err != nil {
			return fmt.Errorf("parse JWKS key %q: %w", key.Kid, err)
		}
		keys[key.Kid] = public
	}
	a.keys = keys
	a.lastRefresh = time.Now()
	return nil
}

func (a *Authenticator) fetchJSON(ctx context.Context, endpoint string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %s", endpoint, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, fetchLimit))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, target)
}

func rsaPublicKey(key jwksKey) (*rsa.PublicKey, error) {
	modulus, err := base64.RawURLEncoding.DecodeString(key.N)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}
	exponent, err := base64.RawURLEncoding.DecodeString(key.E)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}
	if len(modulus) == 0 || len(exponent) == 0 || len(exponent) > 8 {
		return nil, errors.New("malformed RSA key material")
	}
	e := new(big.Int).SetBytes(exponent)
	if !e.IsInt64() || e.Int64() < 3 {
		return nil, errors.New("malformed RSA exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(e.Int64())}, nil
}
