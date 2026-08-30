package fleet

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type fakeSocket struct {
	mu     sync.Mutex
	reads  chan []byte
	writes chan []byte
	closed chan struct{}
}

func newFakeSocket(messages ...any) *fakeSocket {
	socket := &fakeSocket{
		reads:  make(chan []byte, len(messages)+4),
		writes: make(chan []byte, 8),
		closed: make(chan struct{}),
	}
	for _, message := range messages {
		data, _ := json.Marshal(message)
		socket.reads <- data
	}
	return socket
}

func (s *fakeSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-s.closed:
		return 0, nil, errors.New("closed")
	case data := <-s.reads:
		return websocket.MessageText, data, nil
	}
}

func (s *fakeSocket) Write(ctx context.Context, _ websocket.MessageType, data []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closed:
		return errors.New("closed")
	case s.writes <- append([]byte(nil), data...):
		return nil
	}
}

func (s *fakeSocket) Close(websocket.StatusCode, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

func TestConnectorHelloHeartbeatAndRevoke(t *testing.T) {
	store, publicKey := connectorTestStorage(t)
	challenge := Challenge{
		Type:            "challenge",
		ProtocolVersion: ProtocolVersion,
		FleetID:         "fleet-1",
		ConnectionID:    "connection-1",
		Nonce:           "nonce-1",
		SentAt:          time.Now(),
	}
	socket := newFakeSocket(
		challenge,
		HelloAck{Type: "hello-ack", ConnectionID: "connection-1", HeartbeatSeconds: 30},
	)
	connector := NewConnector(store, "root", "v1.2.3")
	connector.HeartbeatOverride = 5 * time.Millisecond
	connector.Dial = func(_ context.Context, endpoint string, options *websocket.DialOptions) (Socket, *http.Response, error) {
		if endpoint != "wss://fleet.example/api/fleet/v1/connections" {
			t.Fatalf("endpoint = %q", endpoint)
		}
		if options.HTTPHeader.Get("Authorization") != "Bearer credential-secret" ||
			options.HTTPHeader.Get("X-Goobers-Instance-Id") != "instance-1" {
			t.Fatalf("headers = %v", options.HTTPHeader)
		}
		return socket, nil, nil
	}
	done := make(chan error, 1)
	go func() { done <- connector.Run(context.Background()) }()

	var hello Hello
	select {
	case data := <-socket.writes:
		if err := json.Unmarshal(data, &hello); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for hello")
	}
	if hello.Type != "hello" || hello.ACL.PolicyVersion != ProtocolVersion || hello.GoobersVersion != "v1.2.3" {
		t.Fatalf("hello = %+v", hello)
	}
	signature, err := base64.RawURLEncoding.DecodeString(hello.Signature)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(ChallengePayload(challenge.FleetID, "registration-1", 3, challenge.ConnectionID, challenge.Nonce)))
	if !ecdsa.VerifyASN1(publicKey, digest[:], signature) {
		t.Fatal("hello signature did not verify")
	}

	var heartbeat Heartbeat
	select {
	case data := <-socket.writes:
		if err := json.Unmarshal(data, &heartbeat); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for heartbeat")
	}
	if heartbeat.Type != "heartbeat" || heartbeat.ACLVersion != ProtocolVersion {
		t.Fatalf("heartbeat = %+v", heartbeat)
	}
	heartbeatAck, _ := json.Marshal(HeartbeatAck{Type: "heartbeat-ack", ConnectionID: "connection-1"})
	socket.reads <- heartbeatAck
	revoke, _ := json.Marshal(Revoke{Type: "revoke", Reason: "operator removed registration"})
	socket.reads <- revoke
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("connector did not stop after revoke")
	}
	record, err := store.Load("root")
	if err != nil {
		t.Fatal(err)
	}
	if !record.Association.Revoked || record.Association.RevokeReason != "operator removed registration" ||
		record.Association.Connected {
		t.Fatalf("association after revoke = %+v", record.Association)
	}
}

func TestConnectorReconnectsAfterFailure(t *testing.T) {
	store, _ := connectorTestStorage(t)
	socket := newFakeSocket(
		Challenge{Type: "challenge", ProtocolVersion: ProtocolVersion, FleetID: "fleet-1", ConnectionID: "connection-2", Nonce: "nonce"},
		HelloAck{Type: "hello-ack", ConnectionID: "connection-2", HeartbeatSeconds: 30},
		Revoke{Type: "revoke", Reason: "done"},
	)
	dials := 0
	connector := NewConnector(store, "root", "dev")
	connector.Dial = func(context.Context, string, *websocket.DialOptions) (Socket, *http.Response, error) {
		dials++
		if dials == 1 {
			return nil, nil, errors.New("temporary outage")
		}
		return socket, nil, nil
	}
	connector.Backoff = func(int) time.Duration { return 0 }
	connector.Wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	if err := connector.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dials != 2 {
		t.Fatalf("dials = %d, want 2", dials)
	}
}

func TestConnectorRedactsCredentialFromDurableError(t *testing.T) {
	store, _ := connectorTestStorage(t)
	connector := NewConnector(store, "root", "dev")
	connector.Dial = func(context.Context, string, *websocket.DialOptions) (Socket, *http.Response, error) {
		return nil, nil, errors.New("server echoed credential-secret")
	}
	connector.Backoff = func(int) time.Duration { return 0 }
	connector.Wait = func(context.Context, time.Duration) error { return context.Canceled }
	if err := connector.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	record, err := store.Load("root")
	if err != nil {
		t.Fatal(err)
	}
	if record.Association.LastError != "fleet: connect: server echoed <redacted>" {
		t.Fatalf("last error = %q", record.Association.LastError)
	}
}

func TestConnectorCancellationStopsBlockedConnection(t *testing.T) {
	store, _ := connectorTestStorage(t)
	socket := newFakeSocket(
		Challenge{Type: "challenge", ProtocolVersion: ProtocolVersion, FleetID: "fleet-1", ConnectionID: "connection", Nonce: "nonce"},
		HelloAck{Type: "hello-ack", ConnectionID: "connection", HeartbeatSeconds: 30},
	)
	connector := NewConnector(store, "root", "dev")
	connector.Dial = func(context.Context, string, *websocket.DialOptions) (Socket, *http.Response, error) {
		return socket, nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- connector.Run(ctx) }()
	select {
	case <-socket.writes:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for hello")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("connector did not stop after cancellation")
	}
}

func TestConnectorStopsReconnectAfterAssociationRemoved(t *testing.T) {
	store, _ := connectorTestStorage(t)
	socket := newFakeSocket(
		Challenge{Type: "challenge", ProtocolVersion: ProtocolVersion, FleetID: "fleet-1", ConnectionID: "connection", Nonce: "nonce"},
		HelloAck{Type: "hello-ack", ConnectionID: "connection", HeartbeatSeconds: 30},
	)
	dials := 0
	connector := NewConnector(store, "root", "dev")
	connector.HeartbeatOverride = 5 * time.Millisecond
	connector.Dial = func(context.Context, string, *websocket.DialOptions) (Socket, *http.Response, error) {
		dials++
		return socket, nil, nil
	}
	done := make(chan error, 1)
	go func() { done <- connector.Run(context.Background()) }()
	select {
	case <-socket.writes:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for hello")
	}
	if err := store.Delete("root"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("connector did not stop after association removal")
	}
	if dials != 1 {
		t.Fatalf("dials = %d, want no reconnect after leave", dials)
	}
}

func connectorTestStorage(t *testing.T) (*memoryStorage, *ecdsa.PublicKey) {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := MarshalPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStorage{
		saved: true,
		record: Record{
			Association: Association{
				SchemaVersion:          ProtocolVersion,
				InstanceID:             "instance-1",
				DisplayName:            "dev-box",
				FleetID:                "fleet-1",
				RegistrationID:         "registration-1",
				RegistrationGeneration: 3,
				CanonicalURI:           "https://fleet.example",
				ConnectionEndpoint:     "wss://fleet.example/api/fleet/v1/connections",
				CredentialExpiresAt:    time.Now().Add(time.Hour),
				ProtocolVersion:        ProtocolVersion,
				ACL:                    ACL{PolicyVersion: ProtocolVersion, Grants: []Grant{}},
			},
			PrivateKey: privateKey,
			Credential: "credential-secret",
		},
	}
	spki, err := PublicKeySPKI(key)
	if err != nil {
		t.Fatal(err)
	}
	der, err := base64.StdEncoding.DecodeString(spki)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		t.Fatal(err)
	}
	return store, parsed.(*ecdsa.PublicKey)
}
