package fleet

import (
	"errors"
	"time"
)

const ProtocolVersion = "1"

var ErrNotAssociated = errors.New("fleet: instance is not associated")

type Principal struct {
	Kind    string `json:"kind"`
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

type Grant struct {
	Principal    Principal `json:"principal"`
	Capabilities []string  `json:"capabilities"`
}

type ACL struct {
	PolicyVersion string  `json:"policyVersion"`
	Grants        []Grant `json:"grants"`
}

type Discovery struct {
	FleetID                        string     `json:"fleetId"`
	CanonicalURI                   string     `json:"canonicalUri"`
	ProtocolVersions               []string   `json:"protocolVersions"`
	EnrollmentEndpoint             string     `json:"enrollmentEndpoint"`
	ConnectionEndpoint             string     `json:"connectionEndpoint"`
	SupportedAuthenticationMethods []string   `json:"supportedAuthenticationMethods"`
	LocalAdministratorPrincipal    *Principal `json:"localAdministratorPrincipal,omitempty"`
}

type EnrollmentRequest struct {
	Grant           string `json:"grant"`
	InstanceID      string `json:"instanceId"`
	DisplayName     string `json:"displayName"`
	PublicKeySPKI   string `json:"publicKeySpki"`
	ProtocolVersion string `json:"protocolVersion"`
	ACL             ACL    `json:"acl"`
}

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

type Record struct {
	Association Association
	PrivateKey  []byte
	Credential  string
}

type Challenge struct {
	Type            string    `json:"type"`
	ProtocolVersion string    `json:"protocolVersion"`
	FleetID         string    `json:"fleetId"`
	ConnectionID    string    `json:"connectionId"`
	Nonce           string    `json:"nonce"`
	SentAt          time.Time `json:"sentAt"`
}

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

type HelloAck struct {
	Type             string `json:"type"`
	ConnectionID     string `json:"connectionId"`
	HeartbeatSeconds int    `json:"heartbeatSeconds"`
}

type Heartbeat struct {
	Type         string    `json:"type"`
	ConnectionID string    `json:"connectionId"`
	SentAt       time.Time `json:"sentAt"`
	ACLVersion   string    `json:"aclVersion"`
}

type HeartbeatAck struct {
	Type         string    `json:"type"`
	ConnectionID string    `json:"connectionId"`
	ReceivedAt   time.Time `json:"receivedAt,omitempty"`
}

type Revoke struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}
