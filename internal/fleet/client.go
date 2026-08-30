// Package fleet implements the Goobers Fleet enrollment and connection
// protocol: discovering a Fleet service, joining it, and persisting the
// resulting durable association, private key, and credential.
package fleet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
)

const maxResponseBytes = 1 << 20

// HTTPDoer is the subset of *http.Client used by Client, allowing callers to
// substitute a custom transport (for example, in tests).
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client is a Fleet enrollment client that performs discovery, enrollment,
// and join operations against a Fleet service.
type Client struct {
	HTTP HTTPDoer
	Now  func() time.Time
}

// JoinOptions configures a Join or JoinDiscovered call.
type JoinOptions struct {
	Grant        string
	InstanceRoot string
	DisplayName  string
	ACL          ACL
}

// Discover fetches and validates the Fleet service's well-known discovery
// document at fleetURL.
func (c Client) Discover(ctx context.Context, fleetURL string) (Discovery, error) {
	base, err := url.Parse(strings.TrimSpace(fleetURL))
	if err != nil || !base.IsAbs() || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return Discovery{}, fmt.Errorf("fleet: URL must be an absolute http or https URL")
	}
	discoveryURL := *base
	discoveryURL.Path = path.Join("/", ".well-known", "goobers-fleet")
	discoveryURL.RawPath = ""
	discoveryURL.RawQuery = ""
	discoveryURL.Fragment = ""
	var discovery Discovery
	if err := c.doJSON(ctx, http.MethodGet, discoveryURL.String(), nil, &discovery); err != nil {
		return Discovery{}, fmt.Errorf("fleet: discovery: %w", err)
	}
	if err := validateDiscovery(discovery); err != nil {
		return Discovery{}, err
	}
	return discovery, nil
}

// Enroll redeems an enrollment grant at endpoint and returns the resulting
// enrollment response, validating that it is complete and protocol-compatible.
func (c Client) Enroll(ctx context.Context, endpoint string, request EnrollmentRequest) (EnrollmentResponse, error) {
	var response EnrollmentResponse
	if err := c.doJSON(ctx, http.MethodPost, endpoint, request, &response); err != nil {
		return EnrollmentResponse{}, fmt.Errorf("fleet: enrollment: %w", err)
	}
	if response.ProtocolVersion != ProtocolVersion {
		return EnrollmentResponse{}, fmt.Errorf("fleet: enrollment selected unsupported protocol version %q", response.ProtocolVersion)
	}
	if response.FleetID == "" || response.RegistrationID == "" || response.RegistrationGeneration <= 0 ||
		response.CanonicalURI == "" || response.ConnectionEndpoint == "" ||
		response.Credential == "" || response.CredentialExpiresAt.IsZero() {
		return EnrollmentResponse{}, fmt.Errorf("fleet: enrollment response is incomplete")
	}
	if err := validateEndpoint(response.ConnectionEndpoint, []string{"ws", "wss"}, "/api/fleet/v1/connections"); err != nil {
		return EnrollmentResponse{}, fmt.Errorf("fleet: enrollment connection endpoint: %w", err)
	}
	return response, nil
}

// JoinDiscovered enrolls the instance with an already-discovered Fleet
// service, generates a new instance key, and persists the resulting
// association to storage.
func (c Client) JoinDiscovered(ctx context.Context, storage Storage, discovery Discovery, options JoinOptions) (Association, error) {
	if strings.TrimSpace(options.Grant) == "" {
		return Association{}, fmt.Errorf("fleet: enrollment grant must not be empty")
	}
	if err := validateDiscovery(discovery); err != nil {
		return Association{}, err
	}
	if _, err := storage.Load(options.InstanceRoot); err == nil {
		return Association{}, fmt.Errorf("fleet: instance is already associated; leave it before joining another Fleet")
	} else if !errors.Is(err, ErrNotAssociated) {
		return Association{}, err
	}
	key, err := GenerateKey()
	if err != nil {
		return Association{}, err
	}
	privateKey, err := MarshalPrivateKey(key)
	if err != nil {
		return Association{}, err
	}
	publicKey, err := PublicKeySPKI(key)
	if err != nil {
		return Association{}, err
	}
	instanceID, err := randomUUID()
	if err != nil {
		return Association{}, err
	}
	response, err := c.Enroll(ctx, discovery.EnrollmentEndpoint, EnrollmentRequest{
		Grant:           options.Grant,
		InstanceID:      instanceID,
		DisplayName:     options.DisplayName,
		PublicKeySPKI:   publicKey,
		ProtocolVersion: ProtocolVersion,
		ACL:             options.ACL,
	})
	if err != nil {
		return Association{}, err
	}
	if response.FleetID != discovery.FleetID {
		return Association{}, fmt.Errorf("fleet: enrollment fleet ID %q does not match discovery %q", response.FleetID, discovery.FleetID)
	}
	if response.CanonicalURI != discovery.CanonicalURI {
		return Association{}, fmt.Errorf("fleet: enrollment canonical URI %q does not match discovery %q", response.CanonicalURI, discovery.CanonicalURI)
	}
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	association := Association{
		SchemaVersion:          ProtocolVersion,
		InstanceID:             instanceID,
		DisplayName:            options.DisplayName,
		FleetID:                response.FleetID,
		RegistrationID:         response.RegistrationID,
		RegistrationGeneration: response.RegistrationGeneration,
		CanonicalURI:           response.CanonicalURI,
		ConnectionEndpoint:     response.ConnectionEndpoint,
		CredentialExpiresAt:    response.CredentialExpiresAt,
		ProtocolVersion:        response.ProtocolVersion,
		ACL:                    options.ACL,
		JoinedAt:               now.UTC(),
	}
	if err := storage.Save(options.InstanceRoot, Record{
		Association: association,
		PrivateKey:  privateKey,
		Credential:  response.Credential,
	}); err != nil {
		return Association{}, err
	}
	return association, nil
}

func validateDiscovery(discovery Discovery) error {
	if discovery.FleetID == "" || discovery.CanonicalURI == "" {
		return fmt.Errorf("fleet: discovery response is incomplete")
	}
	if !slices.Contains(discovery.ProtocolVersions, ProtocolVersion) {
		return fmt.Errorf("fleet: discovery does not support protocol version %s", ProtocolVersion)
	}
	if !slices.Contains(discovery.SupportedAuthenticationMethods, "enrollment-grant") {
		return fmt.Errorf("fleet: discovery does not support enrollment-grant authentication")
	}
	if err := validateEndpoint(discovery.EnrollmentEndpoint, []string{"http", "https"}, "/api/fleet/v1/enrollments:redeem"); err != nil {
		return fmt.Errorf("fleet: enrollment endpoint: %w", err)
	}
	if err := validateEndpoint(discovery.ConnectionEndpoint, []string{"ws", "wss"}, "/api/fleet/v1/connections"); err != nil {
		return fmt.Errorf("fleet: connection endpoint: %w", err)
	}
	if canonical, err := url.Parse(discovery.CanonicalURI); err != nil || !canonical.IsAbs() || canonical.Host == "" {
		return fmt.Errorf("fleet: canonical URI must be absolute")
	}
	if principal := discovery.LocalAdministratorPrincipal; principal != nil {
		if principal.Kind != "user" || principal.Issuer == "" || principal.Subject == "" {
			return fmt.Errorf("fleet: local administrator principal is invalid")
		}
	}
	return nil
}

func validateEndpoint(raw string, schemes []string, suffix string) error {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || !slices.Contains(schemes, parsed.Scheme) {
		return fmt.Errorf("must be an absolute %s URI", strings.Join(schemes, "/"))
	}
	if !strings.HasSuffix(strings.TrimSuffix(parsed.Path, "/"), suffix) {
		return fmt.Errorf("must end with %s", suffix)
	}
	return nil
}

func (c Client) doJSON(ctx context.Context, method, endpoint string, requestBody any, responseBody any) error {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("server returned %s", response.Status)
	}
	if err := json.Unmarshal(data, responseBody); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func randomUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("fleet: generate instance ID: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
