package fleet

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

var errRevoked = errors.New("fleet: registration revoked")
var errAssociationChanged = errors.New("fleet: association changed")

// Socket is the WebSocket surface used by Connector.
type Socket interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Close(websocket.StatusCode, string) error
}

// DialFunc opens a Fleet WebSocket connection.
type DialFunc func(context.Context, string, *websocket.DialOptions) (Socket, *http.Response, error)

// Connector maintains an enrolled instance's authenticated outbound Fleet
// connection until its context is canceled, association is removed, or
// registration is revoked.
type Connector struct {
	Storage           Storage
	InstanceRoot      string
	GoobersVersion    string
	Dial              DialFunc
	Now               func() time.Time
	Backoff           func(int) time.Duration
	Wait              func(context.Context, time.Duration) error
	HeartbeatOverride time.Duration
}

// NewConnector constructs a Connector for one instance root.
func NewConnector(storage Storage, instanceRoot, goobersVersion string) *Connector {
	return &Connector{
		Storage:        storage,
		InstanceRoot:   instanceRoot,
		GoobersVersion: goobersVersion,
	}
}

// Run connects and reconnects until the context is canceled, the association
// is removed, or the Fleet service revokes the registration.
func (c *Connector) Run(ctx context.Context) error {
	if c.Storage == nil {
		return fmt.Errorf("fleet: connector storage is required")
	}
	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		record, err := c.Storage.Load(c.InstanceRoot)
		if errors.Is(err, ErrNotAssociated) {
			return nil
		}
		if err != nil {
			return err
		}
		if record.Association.Revoked {
			return nil
		}
		if err := c.recordDisconnected(record.Association.RegistrationID, nil); errors.Is(err, errAssociationChanged) ||
			errors.Is(err, ErrNotAssociated) {
			return nil
		} else if err != nil {
			return err
		}
		if !record.Association.CredentialExpiresAt.After(c.now()) {
			err := fmt.Errorf("fleet: credential expired at %s", record.Association.CredentialExpiresAt.UTC().Format(time.RFC3339))
			_ = c.recordDisconnected(record.Association.RegistrationID, err)
			return err
		}
		err = c.connectOnce(ctx, record)
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrNotAssociated) ||
			errors.Is(err, errAssociationChanged) || errors.Is(err, errRevoked) {
			return nil
		}
		safeErr := redactCredential(err, record.Credential)
		_ = c.recordDisconnected(record.Association.RegistrationID, safeErr)
		delay := c.backoff(attempt)
		attempt++
		if err := c.wait(ctx, delay); err != nil {
			return nil
		}
	}
}

func redactCredential(err error, credential string) error {
	if err == nil || credential == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), credential, "<redacted>"))
}

func (c *Connector) connectOnce(ctx context.Context, record Record) error {
	key, err := ParsePrivateKey(record.PrivateKey)
	if err != nil {
		return err
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+record.Credential)
	headers.Set("X-Goobers-Instance-Id", record.Association.InstanceID)
	dial := c.Dial
	if dial == nil {
		dial = func(ctx context.Context, endpoint string, options *websocket.DialOptions) (Socket, *http.Response, error) {
			conn, response, err := websocket.Dial(ctx, endpoint, options)
			return conn, response, err
		}
	}
	socket, _, err := dial(ctx, record.Association.ConnectionEndpoint, &websocket.DialOptions{
		HTTPHeader: headers,
	})
	if err != nil {
		return fmt.Errorf("fleet: connect: %w", err)
	}
	defer func() { _ = socket.Close(websocket.StatusNormalClosure, "") }()

	var challenge Challenge
	if err := readTypedMessage(ctx, socket, "challenge", &challenge); err != nil {
		return err
	}
	if challenge.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("fleet: challenge selected unsupported protocol version %q", challenge.ProtocolVersion)
	}
	if challenge.FleetID != record.Association.FleetID {
		return fmt.Errorf("fleet: challenge fleet ID %q does not match association %q", challenge.FleetID, record.Association.FleetID)
	}
	if challenge.ConnectionID == "" || challenge.Nonce == "" {
		return fmt.Errorf("fleet: challenge is incomplete")
	}
	signature, err := SignChallenge(
		key,
		challenge.FleetID,
		record.Association.RegistrationID,
		record.Association.RegistrationGeneration,
		challenge.ConnectionID,
		challenge.Nonce,
	)
	if err != nil {
		return err
	}
	if err := writeMessage(ctx, socket, Hello{
		Type:                   "hello",
		ProtocolVersion:        ProtocolVersion,
		InstanceID:             record.Association.InstanceID,
		RegistrationID:         record.Association.RegistrationID,
		RegistrationGeneration: record.Association.RegistrationGeneration,
		ConnectionID:           challenge.ConnectionID,
		Nonce:                  challenge.Nonce,
		Signature:              signature,
		DisplayName:            record.Association.DisplayName,
		GoobersVersion:         c.GoobersVersion,
		ACL:                    record.Association.ACL,
	}); err != nil {
		return err
	}
	var ack HelloAck
	if err := readTypedMessage(ctx, socket, "hello-ack", &ack); err != nil {
		return err
	}
	if ack.ConnectionID != challenge.ConnectionID {
		return fmt.Errorf("fleet: hello acknowledgement connection ID %q does not match challenge %q", ack.ConnectionID, challenge.ConnectionID)
	}
	if ack.HeartbeatSeconds <= 0 && c.HeartbeatOverride <= 0 {
		return fmt.Errorf("fleet: hello acknowledgement heartbeatSeconds must be positive")
	}
	connectedAt := c.now()
	if err := c.Storage.Update(c.InstanceRoot, func(association *Association) error {
		if association.RegistrationID != record.Association.RegistrationID {
			return errAssociationChanged
		}
		association.Connected = true
		association.ConnectionID = challenge.ConnectionID
		association.LastConnectedAt = connectedAt
		association.LastError = ""
		return nil
	}); err != nil {
		return err
	}
	defer func() { _ = c.recordDisconnected(record.Association.RegistrationID, nil) }()

	heartbeatInterval := time.Duration(ack.HeartbeatSeconds) * time.Second
	if c.HeartbeatOverride > 0 {
		heartbeatInterval = c.HeartbeatOverride
	}
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	messages := make(chan inboundMessage, 1)
	go func() {
		send := func(message inboundMessage) bool {
			select {
			case messages <- message:
				return true
			case <-readCtx.Done():
				return false
			}
		}
		for {
			messageType, data, readErr := socket.Read(readCtx)
			if readErr != nil {
				send(inboundMessage{err: readErr})
				return
			}
			if messageType != websocket.MessageText {
				send(inboundMessage{err: fmt.Errorf("fleet: inbound message must be text")})
				return
			}
			var envelope struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				send(inboundMessage{err: fmt.Errorf("fleet: decode message: %w", err)})
				return
			}
			if !send(inboundMessage{kind: envelope.Type, data: data}) {
				return
			}
		}
	}()
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case message := <-messages:
			if message.err != nil {
				return fmt.Errorf("fleet: read connection: %w", message.err)
			}
			switch message.kind {
			case "heartbeat-ack":
				var heartbeatAck HeartbeatAck
				if err := json.Unmarshal(message.data, &heartbeatAck); err != nil {
					return fmt.Errorf("fleet: decode heartbeat acknowledgement: %w", err)
				}
				if heartbeatAck.ConnectionID != challenge.ConnectionID {
					return fmt.Errorf("fleet: heartbeat acknowledgement connection ID %q does not match %q", heartbeatAck.ConnectionID, challenge.ConnectionID)
				}
				heartbeatAt := c.now()
				if err := c.Storage.Update(c.InstanceRoot, func(association *Association) error {
					if association.RegistrationID != record.Association.RegistrationID {
						return errAssociationChanged
					}
					association.LastHeartbeatAt = heartbeatAt
					association.LastError = ""
					return nil
				}); err != nil {
					return err
				}
			case "revoke":
				var revoke Revoke
				if err := json.Unmarshal(message.data, &revoke); err != nil {
					return fmt.Errorf("fleet: decode revoke: %w", err)
				}
				if err := c.Storage.Update(c.InstanceRoot, func(association *Association) error {
					if association.RegistrationID != record.Association.RegistrationID {
						return errAssociationChanged
					}
					association.Revoked = true
					association.RevokeReason = strings.TrimSpace(revoke.Reason)
					association.Connected = false
					association.ConnectionID = ""
					association.LastError = ""
					return nil
				}); err != nil {
					return err
				}
				return errRevoked
			default:
				return fmt.Errorf("fleet: unexpected message type %q", message.kind)
			}
		case <-ticker.C:
			current, err := c.Storage.Load(c.InstanceRoot)
			if err != nil {
				return err
			}
			if current.Association.Revoked {
				return errRevoked
			}
			if current.Association.InstanceID != record.Association.InstanceID ||
				current.Association.RegistrationID != record.Association.RegistrationID ||
				current.Association.RegistrationGeneration != record.Association.RegistrationGeneration {
				return errAssociationChanged
			}
			if err := writeMessage(ctx, socket, Heartbeat{
				Type:         "heartbeat",
				ConnectionID: challenge.ConnectionID,
				SentAt:       c.now(),
				ACLVersion:   current.Association.ACL.PolicyVersion,
			}); err != nil {
				return err
			}
		}
	}
}

type inboundMessage struct {
	kind string
	data []byte
	err  error
}

func readTypedMessage(ctx context.Context, socket Socket, expected string, destination any) error {
	messageType, data, err := socket.Read(ctx)
	if err != nil {
		return fmt.Errorf("fleet: read %s: %w", expected, err)
	}
	if messageType != websocket.MessageText {
		return fmt.Errorf("fleet: %s must be a text message", expected)
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("fleet: decode %s: %w", expected, err)
	}
	if envelope.Type != expected {
		return fmt.Errorf("fleet: expected %s, received %q", expected, envelope.Type)
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return fmt.Errorf("fleet: decode %s: %w", expected, err)
	}
	return nil
}

func writeMessage(ctx context.Context, socket Socket, message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("fleet: encode message: %w", err)
	}
	if err := socket.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("fleet: write message: %w", err)
	}
	return nil
}

func (c *Connector) recordDisconnected(registrationID string, connectionErr error) error {
	return c.Storage.Update(c.InstanceRoot, func(association *Association) error {
		if association.RegistrationID != registrationID {
			return errAssociationChanged
		}
		association.Connected = false
		association.ConnectionID = ""
		if connectionErr != nil {
			association.LastError = connectionErr.Error()
		}
		return nil
	})
}

func (c *Connector) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *Connector) backoff(attempt int) time.Duration {
	if c.Backoff != nil {
		return c.Backoff(attempt)
	}
	if attempt > 5 {
		attempt = 5
	}
	base := time.Second << attempt
	if base > 30*time.Second {
		base = 30 * time.Second
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return base
	}
	fraction := float64(binary.LittleEndian.Uint64(random[:])) / float64(^uint64(0))
	return time.Duration(float64(base) * (0.75 + 0.5*fraction))
}

func (c *Connector) wait(ctx context.Context, delay time.Duration) error {
	if c.Wait != nil {
		return c.Wait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
