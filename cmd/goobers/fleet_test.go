package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/fleet"
	"github.com/goobers/goobers/internal/instance"
)

type fleetMemoryStorage struct {
	record               fleet.Record
	saved                bool
	loadCalls            int
	loadAssociationCalls int
}

func (s *fleetMemoryStorage) LoadAssociation(string) (fleet.Association, error) {
	s.loadAssociationCalls++
	if !s.saved {
		return fleet.Association{}, fleet.ErrNotAssociated
	}
	return s.record.Association, nil
}

func (s *fleetMemoryStorage) Load(string) (fleet.Record, error) {
	s.loadCalls++
	if !s.saved {
		return fleet.Record{}, fleet.ErrNotAssociated
	}
	return s.record, nil
}
func (s *fleetMemoryStorage) Save(_ string, record fleet.Record) error {
	s.record = record
	s.saved = true
	return nil
}
func (s *fleetMemoryStorage) Update(_ string, update func(*fleet.Association) error) error {
	if !s.saved {
		return fleet.ErrNotAssociated
	}
	return update(&s.record.Association)
}
func (s *fleetMemoryStorage) Delete(string) error {
	if !s.saved {
		return fleet.ErrNotAssociated
	}
	s.saved = false
	return nil
}

func TestFleetJoinTokenFileAndLocalAdminGrant(t *testing.T) {
	root := t.TempDir()
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "grant.txt")
	if err := os.WriteFile(tokenFile, []byte("secret-grant\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var enrollment fleet.EnrollmentRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/goobers-fleet":
			_ = json.NewEncoder(w).Encode(fleet.Discovery{
				FleetID:                        "fleet-1",
				CanonicalURI:                   "https://fleet.example",
				ProtocolVersions:               []string{fleet.ProtocolVersion},
				EnrollmentEndpoint:             cliFleetServerURL(r) + "/api/fleet/v1/enrollments:redeem",
				ConnectionEndpoint:             "wss://fleet.example/api/fleet/v1/connections",
				SupportedAuthenticationMethods: []string{"enrollment-grant"},
				LocalAdministratorPrincipal:    &fleet.Principal{Kind: "user", Issuer: "issuer", Subject: "subject"},
			})
		case "/api/fleet/v1/enrollments:redeem":
			if err := json.NewDecoder(r.Body).Decode(&enrollment); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(fleet.EnrollmentResponse{
				FleetID:                "fleet-1",
				RegistrationID:         "registration-1",
				RegistrationGeneration: 1,
				CanonicalURI:           "https://fleet.example",
				ConnectionEndpoint:     "wss://fleet.example/api/fleet/v1/connections",
				Credential:             "credential",
				CredentialExpiresAt:    time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
				ProtocolVersion:        fleet.ProtocolVersion,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	store := &fleetMemoryStorage{}
	originalStorage := newFleetStorage
	originalHTTP := fleetHTTPClient
	newFleetStorage = func() (fleet.Storage, error) { return store, nil }
	fleetHTTPClient = server.Client()
	t.Cleanup(func() {
		newFleetStorage = originalStorage
		fleetHTTPClient = originalHTTP
	})

	var stdout, stderr strings.Builder
	code := runFleetJoinWithInput(context.Background(), []string{
		"--url", server.URL,
		"--enrollment-token-file", tokenFile,
		"--grant-local-admin",
		root,
	}, strings.NewReader(""), &stdout, &stderr, false)
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if enrollment.Grant != "secret-grant" || len(enrollment.ACL.Grants) != 1 ||
		enrollment.ACL.Grants[0].Capabilities[0] != "instance:read" {
		t.Fatalf("enrollment = %+v", enrollment)
	}
	if strings.Contains(stdout.String(), "secret-grant") || strings.Contains(stderr.String(), "secret-grant") {
		t.Fatal("CLI output exposed enrollment grant")
	}
}

func TestFleetJoinNoninteractiveRequiresACLChoice(t *testing.T) {
	root := t.TempDir()
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	server := fleetDiscoveryServer(t, &fleet.Principal{Kind: "user", Issuer: "issuer", Subject: "subject"})
	defer server.Close()
	originalHTTP := fleetHTTPClient
	fleetHTTPClient = server.Client()
	t.Cleanup(func() { fleetHTTPClient = originalHTTP })

	var stdout, stderr strings.Builder
	code := runFleetJoinWithInput(context.Background(), []string{"--url", server.URL, root},
		strings.NewReader(""), &stdout, &stderr, false)
	if code != 2 || !strings.Contains(stderr.String(), "--grant-local-admin or --no-grant-local-admin") {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}

func TestFleetJoinInteractiveEmptyACLRequiresWarningConfirmation(t *testing.T) {
	discovery := fleet.Discovery{}
	var stdout strings.Builder
	acl, err := selectFleetACL(discovery, false, false, true, strings.NewReader("yes\n"), &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if len(acl.Grants) != 0 || !strings.Contains(stdout.String(), "empty ACL") {
		t.Fatalf("acl=%+v stdout=%q", acl, stdout.String())
	}

	stdout.Reset()
	if _, err := selectFleetACL(discovery, false, false, true, strings.NewReader("no\n"), &stdout); err == nil {
		t.Fatal("empty ACL was accepted without confirmation")
	}
}

func TestFleetStatusJSONRedactsSecretsAndLeaveDeletes(t *testing.T) {
	root := t.TempDir()
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	store := &fleetMemoryStorage{
		saved: true,
		record: fleet.Record{
			Association: fleet.Association{
				InstanceID:          "instance-1",
				DisplayName:         "dev-box",
				FleetID:             "fleet-1",
				RegistrationID:      "registration-1",
				CanonicalURI:        "https://fleet.example",
				ConnectionEndpoint:  "wss://fleet.example/api/fleet/v1/connections",
				ProtocolVersion:     fleet.ProtocolVersion,
				ACL:                 fleet.ACL{PolicyVersion: fleet.ProtocolVersion},
				CredentialExpiresAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
				Connected:           true,
				ConnectionID:        "connection-1",
				HeartbeatSeconds:    10,
				LastHeartbeatAt:     time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
			},
			PrivateKey: []byte("private-secret"),
			Credential: "bearer-secret",
		},
	}
	originalStorage := newFleetStorage
	originalNow := fleetStatusNow
	newFleetStorage = func() (fleet.Storage, error) { return store, nil }
	fleetStatusNow = func() time.Time { return time.Date(2026, 8, 30, 0, 0, 31, 0, time.UTC) }
	t.Cleanup(func() {
		newFleetStorage = originalStorage
		fleetStatusNow = originalNow
	})

	code, stdout, stderr := runArgs(t, "fleet", "status", "--json", root)
	if code != 0 || stderr != "" {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, "private-secret") || strings.Contains(stdout, "bearer-secret") {
		t.Fatalf("status output exposed secrets: %s", stdout)
	}
	var status fleetStatusOutput
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Associated || status.RegistrationID != "registration-1" {
		t.Fatalf("status = %+v", status)
	}
	if status.Connected || !status.Stale || status.ConnectionState != "stale" || status.HeartbeatSeconds != 10 {
		t.Fatalf("stale connection status = %+v", status)
	}
	code, stdout, stderr = runArgs(t, "fleet", "status", root)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "connection: stale") {
		t.Fatalf("text status code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if store.loadAssociationCalls != 2 || store.loadCalls != 0 {
		t.Fatalf("status storage reads: metadata=%d secrets=%d", store.loadAssociationCalls, store.loadCalls)
	}

	code, stdout, stderr = runArgs(t, "fleet", "leave", root)
	if code != 0 || stderr != "" || store.saved {
		t.Fatalf("leave code=%d stdout=%q stderr=%q saved=%v", code, stdout, stderr, store.saved)
	}
}

func TestFleetConnectionStateHeartbeatFreshness(t *testing.T) {
	now := time.Date(2026, 8, 30, 1, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		association fleet.Association
		want        string
	}{
		{name: "disconnected", association: fleet.Association{}, want: "disconnected"},
		{name: "revoked", association: fleet.Association{Connected: true, Revoked: true}, want: "revoked"},
		{
			name: "fresh within three intervals",
			association: fleet.Association{
				Connected:        true,
				HeartbeatSeconds: 20,
				LastHeartbeatAt:  now.Add(-time.Minute),
			},
			want: "connected",
		},
		{
			name: "stale after three intervals",
			association: fleet.Association{
				Connected:        true,
				HeartbeatSeconds: 20,
				LastHeartbeatAt:  now.Add(-time.Minute - time.Nanosecond),
			},
			want: "stale",
		},
		{
			name: "minimum freshness window",
			association: fleet.Association{
				Connected:        true,
				HeartbeatSeconds: 1,
				LastHeartbeatAt:  now.Add(-30*time.Second - time.Nanosecond),
			},
			want: "stale",
		},
		{
			name: "awaiting first heartbeat",
			association: fleet.Association{
				Connected:        true,
				HeartbeatSeconds: 10,
				LastConnectedAt:  now.Add(-29 * time.Second),
			},
			want: "connected",
		},
		{
			name: "missing activity is stale",
			association: fleet.Association{
				Connected:        true,
				HeartbeatSeconds: 10,
			},
			want: "stale",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := fleetConnectionState(test.association, now); got != test.want {
				t.Fatalf("fleetConnectionState = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFleetJoinRejectsPlaintextTokenArgument(t *testing.T) {
	code, _, stderr := runArgs(t, "fleet", "join", "--url", "https://fleet.example", "--enrollment-token", "secret")
	if code != 2 || !strings.Contains(stderr, "flag provided but not defined") {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
}

func fleetDiscoveryServer(t *testing.T, principal *fleet.Principal) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/goobers-fleet" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(fleet.Discovery{
			FleetID:                        "fleet-1",
			CanonicalURI:                   "https://fleet.example",
			ProtocolVersions:               []string{fleet.ProtocolVersion},
			EnrollmentEndpoint:             cliFleetServerURL(r) + "/api/fleet/v1/enrollments:redeem",
			ConnectionEndpoint:             "wss://fleet.example/api/fleet/v1/connections",
			SupportedAuthenticationMethods: []string{"enrollment-grant"},
			LocalAdministratorPrincipal:    principal,
		})
	}))
}

func cliFleetServerURL(r *http.Request) string {
	return "http://" + r.Host
}

var _ fleet.Storage = (*fleetMemoryStorage)(nil)
