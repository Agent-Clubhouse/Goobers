package fleet

import (
	"errors"
	"time"
)

// ProtocolVersion is the Fleet protocol version implemented by this client.
const ProtocolVersion = "1"

// ErrNotAssociated indicates that no Fleet association exists for an
// instance.
var ErrNotAssociated = errors.New("fleet: instance is not associated")

// Principal identifies a Fleet user or service by issuer and subject.
type Principal struct {
	Kind    string `json:"kind"`
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// Grant assigns a set of capabilities to a Principal.
type Grant struct {
	Principal    Principal `json:"principal"`
	Capabilities []string  `json:"capabilities"`
}

// ACL is an access control list of Grants for an instance's Fleet
// association.
type ACL struct {
	PolicyVersion string  `json:"policyVersion"`
	Grants        []Grant `json:"grants"`
}

// Discovery is the Fleet service's well-known discovery document.
type Discovery struct {
	FleetID                        string     `json:"fleetId"`
	CanonicalURI                   string     `json:"canonicalUri"`
	ProtocolVersions               []string   `json:"protocolVersions"`
	EnrollmentEndpoint             string     `json:"enrollmentEndpoint"`
	ConnectionEndpoint             string     `json:"connectionEndpoint"`
	SupportedAuthenticationMethods []string   `json:"supportedAuthenticationMethods"`
	LocalAdministratorPrincipal    *Principal `json:"localAdministratorPrincipal,omitempty"`
}

// EnrollmentRequest is sent to redeem an enrollment grant with a Fleet
// service.
type EnrollmentRequest struct {
	Grant           string `json:"grant"`
	InstanceID      string `json:"instanceId"`
	DisplayName     string `json:"displayName"`
	PublicKeySPKI   string `json:"publicKeySpki"`
	ProtocolVersion string `json:"protocolVersion"`
	ACL             ACL    `json:"acl"`
}

// EnrollmentResponse is returned by a Fleet service after a successful
// enrollment.
type EnrollmentResponse struct {
	FleetID                string    `json:"fleetId"`
	RegistrationID         string    `json:"registrationId"`
	RegistrationGeneration int64     `json:"registrationGeneration"`
	CanonicalURI           string    `json:"canonicalUri"`
	ConnectionEndpoint     string    `json:"connectionEndpoint"`
	Credential             string    `json:"credential"`
	CredentialExpiresAt    time.Time `json:"credentialExpiresAt"`
	ProtocolVersion        string    `json:"protocolVersion"`
}

// Association is an instance's durable Fleet registration and connection
// state.
type Association struct {
	SchemaVersion          string    `json:"schemaVersion"`
	InstanceID             string    `json:"instanceId"`
	DisplayName            string    `json:"displayName"`
	FleetID                string    `json:"fleetId"`
	RegistrationID         string    `json:"registrationId"`
	RegistrationGeneration int64     `json:"registrationGeneration"`
	CanonicalURI           string    `json:"canonicalUri"`
	ConnectionEndpoint     string    `json:"connectionEndpoint"`
	CredentialExpiresAt    time.Time `json:"credentialExpiresAt"`
	ProtocolVersion        string    `json:"protocolVersion"`
	ACL                    ACL       `json:"acl"`
	JoinedAt               time.Time `json:"joinedAt"`
	Revoked                bool      `json:"revoked"`
	RevokeReason           string    `json:"revokeReason,omitempty"`
	Connected              bool      `json:"connected"`
	ConnectionID           string    `json:"connectionId,omitempty"`
	LastConnectedAt        time.Time `json:"lastConnectedAt,omitempty"`
	LastHeartbeatAt        time.Time `json:"lastHeartbeatAt,omitempty"`
	LastError              string    `json:"lastError,omitempty"`
}

// Record bundles an Association with its private key and bearer credential
// for storage.
type Record struct {
	Association Association
	PrivateKey  []byte
	Credential  string
}

// Challenge is sent by a Fleet service over a connection to be signed with
// the instance's private key.
type Challenge struct {
	Type            string    `json:"type"`
	ProtocolVersion string    `json:"protocolVersion"`
	FleetID         string    `json:"fleetId"`
	ConnectionID    string    `json:"connectionId"`
	Nonce           string    `json:"nonce"`
	SentAt          time.Time `json:"sentAt"`
}

// Hello is sent by an instance in response to a Challenge to authenticate a
// connection.
type Hello struct {
	Type                   string `json:"type"`
	ProtocolVersion        string `json:"protocolVersion"`
	InstanceID             string `json:"instanceId"`
	RegistrationID         string `json:"registrationId"`
	RegistrationGeneration int64  `json:"registrationGeneration"`
	ConnectionID           string `json:"connectionId"`
	Nonce                  string `json:"nonce"`
	Signature              string `json:"signature"`
	DisplayName            string `json:"displayName"`
	GoobersVersion         string `json:"goobersVersion"`
	ACL                    ACL    `json:"acl"`
}

// HelloAck is sent by a Fleet service to acknowledge a successful Hello.
type HelloAck struct {
	Type             string `json:"type"`
	ConnectionID     string `json:"connectionId"`
	HeartbeatSeconds int    `json:"heartbeatSeconds"`
}

// Heartbeat is periodically sent by an instance to keep a Fleet connection
// alive and report its ACL version.
type Heartbeat struct {
	Type         string    `json:"type"`
	ConnectionID string    `json:"connectionId"`
	SentAt       time.Time `json:"sentAt"`
	ACLVersion   string    `json:"aclVersion"`
}

// HeartbeatAck is sent by a Fleet service to acknowledge a Heartbeat.
type HeartbeatAck struct {
	Type         string    `json:"type"`
	ConnectionID string    `json:"connectionId"`
	ReceivedAt   time.Time `json:"receivedAt,omitempty"`
}

// Revoke is sent by a Fleet service to notify an instance that its
// association has been revoked.
type Revoke struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}
