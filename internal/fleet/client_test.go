package fleet

import (
	"context"
	"encoding/json"
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

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
