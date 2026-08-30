package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryStorage struct {
	mu     sync.Mutex
	record Record
	saved  bool
}

func (s *memoryStorage) LoadAssociation(string) (Association, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.saved {
		return Association{}, ErrNotAssociated
	}
	return s.record.Association, nil
}

func (s *memoryStorage) Load(string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.saved {
		return Record{}, ErrNotAssociated
	}
	return s.record, nil
}
func (s *memoryStorage) Save(_ string, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record = record
	s.saved = true
	return nil
}
func (s *memoryStorage) Update(_ string, update func(*Association) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.saved {
		return ErrNotAssociated
	}
	return update(&s.record.Association)
}
func (s *memoryStorage) Delete(string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.saved {
		return ErrNotAssociated
	}
	s.saved = false
	return nil
}

func TestDiscoverAndEnroll(t *testing.T) {
	expires := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var enrollment EnrollmentRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/goobers-fleet":
			_ = json.NewEncoder(w).Encode(Discovery{
				FleetID:                        "fleet-1",
				CanonicalURI:                   serverURL(r),
				ProtocolVersions:               []string{ProtocolVersion},
				EnrollmentEndpoint:             serverURL(r) + "/api/fleet/v1/enrollments:redeem",
				ConnectionEndpoint:             strings.Replace(serverURL(r), "http://", "ws://", 1) + "/api/fleet/v1/connections",
				SupportedAuthenticationMethods: []string{"enrollment-grant"},
			})
		case "/api/fleet/v1/enrollments:redeem":
			if r.Method != http.MethodPost {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("Content-Type") != "application/json" {
				http.Error(w, "content type", http.StatusBadRequest)
				return
			}
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &enrollment); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(EnrollmentResponse{
				FleetID:                "fleet-1",
				RegistrationID:         "registration-1",
				RegistrationGeneration: 1,
				CanonicalURI:           serverURL(r),
				ConnectionEndpoint:     strings.Replace(serverURL(r), "http://", "ws://", 1) + "/api/fleet/v1/connections",
				Credential:             "credential-secret",
				CredentialExpiresAt:    expires,
				ProtocolVersion:        ProtocolVersion,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	store := &memoryStorage{}
	client := Client{HTTP: server.Client(), Now: func() time.Time { return time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC) }}
	discovery, err := client.Discover(context.Background(), server.URL+"/ignored/path")
	if err != nil {
		t.Fatal(err)
	}
	association, err := client.JoinDiscovered(context.Background(), store, discovery, JoinOptions{
		Grant:        "one-time-grant",
		InstanceRoot: t.TempDir(),
		DisplayName:  "dev-box",
		ACL: ACL{PolicyVersion: ProtocolVersion, Grants: []Grant{{
			Principal:    Principal{Kind: "user", Issuer: "issuer", Subject: "subject"},
			Capabilities: []string{"instance:read"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if association.RegistrationID != "registration-1" || !store.saved {
		t.Fatalf("association = %+v, saved = %v", association, store.saved)
	}
	if enrollment.Grant != "one-time-grant" || enrollment.ProtocolVersion != ProtocolVersion ||
		enrollment.InstanceID == "" || enrollment.PublicKeySPKI == "" {
		t.Fatalf("enrollment request = %+v", enrollment)
	}
	if store.record.Credential != "credential-secret" || len(store.record.PrivateKey) == 0 {
		t.Fatal("protected record did not receive enrollment secrets")
	}
}

func TestEnrollAcceptsNonExpiringDevelopmentCredential(t *testing.T) {
	response := EnrollmentResponse{
		FleetID:                "fleet-1",
		RegistrationID:         "registration-1",
		RegistrationGeneration: 1,
		CanonicalURI:           "https://fleet.example",
		ConnectionEndpoint:     "wss://fleet.example/api/fleet/v1/connections",
		Credential:             "credential",
		ProtocolVersion:        ProtocolVersion,
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}

	enrollment, err := (Client{HTTP: staticHTTPDoer{response: data}}).Enroll(
		context.Background(),
		"https://fleet.example/api/fleet/v1/enrollments:redeem",
		EnrollmentRequest{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !enrollment.CredentialExpiresAt.IsZero() {
		t.Fatalf("credential expiry = %s, want non-expiring zero value", enrollment.CredentialExpiresAt)
	}
}

func TestDiscoveryRejectsWrongEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Discovery{
			FleetID:                        "fleet-1",
			CanonicalURI:                   serverURL(r),
			ProtocolVersions:               []string{ProtocolVersion},
			EnrollmentEndpoint:             serverURL(r) + "/wrong",
			ConnectionEndpoint:             "wss://fleet.example/api/fleet/v1/connections",
			SupportedAuthenticationMethods: []string{"enrollment-grant"},
		})
	}))
	defer server.Close()
	_, err := (Client{HTTP: server.Client()}).Discover(context.Background(), server.URL)
	if err == nil || !strings.Contains(err.Error(), "enrollments:redeem") {
		t.Fatalf("Discover error = %v", err)
	}
}

func TestTransportURIAllowsPlaintextOnlyForLoopback(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		secure string
		plain  string
		ok     bool
	}{
		{name: "localhost HTTP", raw: "http://localhost:8080", secure: "https", plain: "http", ok: true},
		{name: "IPv4 loopback HTTP", raw: "http://127.0.0.1:8080", secure: "https", plain: "http", ok: true},
		{name: "IPv6 loopback HTTP", raw: "http://[::1]:8080", secure: "https", plain: "http", ok: true},
		{name: "remote HTTPS", raw: "https://fleet.example", secure: "https", plain: "http", ok: true},
		{name: "remote HTTP", raw: "http://fleet.example", secure: "https", plain: "http", ok: false},
		{name: "localhost WS", raw: "ws://localhost:8080/api/fleet/v1/connections", secure: "wss", plain: "ws", ok: true},
		{name: "IPv4 loopback WS", raw: "ws://127.0.0.1:8080/api/fleet/v1/connections", secure: "wss", plain: "ws", ok: true},
		{name: "IPv6 loopback WS", raw: "ws://[::1]:8080/api/fleet/v1/connections", secure: "wss", plain: "ws", ok: true},
		{name: "remote WSS", raw: "wss://fleet.example/api/fleet/v1/connections", secure: "wss", plain: "ws", ok: true},
		{name: "remote WS", raw: "ws://fleet.example/api/fleet/v1/connections", secure: "wss", plain: "ws", ok: false},
		{name: "localhost lookalike", raw: "http://localhost.example", secure: "https", plain: "http", ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateTransportURI(test.raw, test.secure, test.plain, "")
			if (err == nil) != test.ok {
				t.Fatalf("validateTransportURI(%q) error = %v, ok = %v", test.raw, err, test.ok)
			}
		})
	}
}

func TestDiscoverRejectsRemotePlaintextInitialURLBeforeRequest(t *testing.T) {
	client := Client{HTTP: rejectingHTTPDoer{t: t}}
	_, err := client.Discover(context.Background(), "http://fleet.example")
	if err == nil || !strings.Contains(err.Error(), "only for localhost") {
		t.Fatalf("Discover error = %v", err)
	}
}

func TestDiscoverAllowsPlaintextLoopbackInitialURLs(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
		t.Run(host, func(t *testing.T) {
			discovery := Discovery{
				FleetID:                        "fleet-1",
				CanonicalURI:                   "https://fleet.example",
				ProtocolVersions:               []string{ProtocolVersion},
				EnrollmentEndpoint:             "https://fleet.example/api/fleet/v1/enrollments:redeem",
				ConnectionEndpoint:             "wss://fleet.example/api/fleet/v1/connections",
				SupportedAuthenticationMethods: []string{"enrollment-grant"},
			}
			data, err := json.Marshal(discovery)
			if err != nil {
				t.Fatal(err)
			}
			client := Client{HTTP: staticHTTPDoer{response: data}}
			if _, err := client.Discover(context.Background(), "http://"+host+":8080"); err != nil {
				t.Fatalf("Discover loopback URL: %v", err)
			}
		})
	}
}

func TestDiscoveryAllowsPlaintextLoopbackURIs(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
		t.Run(host, func(t *testing.T) {
			discovery := Discovery{
				FleetID:                        "fleet-1",
				CanonicalURI:                   "http://" + host + ":8080",
				ProtocolVersions:               []string{ProtocolVersion},
				EnrollmentEndpoint:             "http://" + host + ":8080/api/fleet/v1/enrollments:redeem",
				ConnectionEndpoint:             "ws://" + host + ":8080/api/fleet/v1/connections",
				SupportedAuthenticationMethods: []string{"enrollment-grant"},
			}
			if err := validateDiscovery(discovery); err != nil {
				t.Fatalf("validateDiscovery loopback URIs: %v", err)
			}
		})
	}
}

func TestDiscoveryRejectsRemotePlaintextURIs(t *testing.T) {
	base := Discovery{
		FleetID:                        "fleet-1",
		CanonicalURI:                   "https://fleet.example",
		ProtocolVersions:               []string{ProtocolVersion},
		EnrollmentEndpoint:             "https://fleet.example/api/fleet/v1/enrollments:redeem",
		ConnectionEndpoint:             "wss://fleet.example/api/fleet/v1/connections",
		SupportedAuthenticationMethods: []string{"enrollment-grant"},
	}
	tests := []struct {
		name   string
		mutate func(*Discovery)
	}{
		{name: "canonical URI", mutate: func(d *Discovery) { d.CanonicalURI = "http://fleet.example" }},
		{name: "enrollment endpoint", mutate: func(d *Discovery) { d.EnrollmentEndpoint = "http://fleet.example/api/fleet/v1/enrollments:redeem" }},
		{name: "connection endpoint", mutate: func(d *Discovery) { d.ConnectionEndpoint = "ws://fleet.example/api/fleet/v1/connections" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			discovery := base
			test.mutate(&discovery)
			if err := validateDiscovery(discovery); err == nil {
				t.Fatalf("validateDiscovery accepted remote plaintext %s", test.name)
			}
		})
	}
}

func TestEnrollmentAllowsPlaintextLoopbackReturnedURIs(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]"} {
		t.Run(host, func(t *testing.T) {
			response := EnrollmentResponse{
				FleetID:                "fleet-1",
				RegistrationID:         "registration-1",
				RegistrationGeneration: 1,
				CanonicalURI:           "http://" + host + ":8080",
				ConnectionEndpoint:     "ws://" + host + ":8080/api/fleet/v1/connections",
				Credential:             "credential",
				CredentialExpiresAt:    time.Now().Add(time.Hour),
				ProtocolVersion:        ProtocolVersion,
			}
			data, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			client := Client{HTTP: staticHTTPDoer{response: data}}
			if _, err := client.Enroll(context.Background(), "https://fleet.example/redeem", EnrollmentRequest{}); err != nil {
				t.Fatalf("Enroll loopback response: %v", err)
			}
		})
	}
}

func TestEnrollmentRejectsRemotePlaintextReturnedURIs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*EnrollmentResponse)
	}{
		{name: "canonical URI", mutate: func(r *EnrollmentResponse) { r.CanonicalURI = "http://fleet.example" }},
		{name: "connection endpoint", mutate: func(r *EnrollmentResponse) { r.ConnectionEndpoint = "ws://fleet.example/api/fleet/v1/connections" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				response := EnrollmentResponse{
					FleetID:                "fleet-1",
					RegistrationID:         "registration-1",
					RegistrationGeneration: 1,
					CanonicalURI:           "https://fleet.example",
					ConnectionEndpoint:     "wss://fleet.example/api/fleet/v1/connections",
					Credential:             "credential",
					CredentialExpiresAt:    time.Now().Add(time.Hour),
					ProtocolVersion:        ProtocolVersion,
				}
				test.mutate(&response)
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			_, err := (Client{HTTP: server.Client()}).Enroll(context.Background(), server.URL, EnrollmentRequest{})
			if err == nil {
				t.Fatalf("Enroll accepted remote plaintext %s", test.name)
			}
		})
	}
}

type rejectingHTTPDoer struct {
	t *testing.T
}

func (d rejectingHTTPDoer) Do(*http.Request) (*http.Response, error) {
	d.t.Fatal("HTTP request should not be attempted")
	return nil, errors.New("unreachable")
}

type staticHTTPDoer struct {
	response []byte
}

func (d staticHTTPDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader(string(d.response))),
		Header:     make(http.Header),
	}, nil
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
