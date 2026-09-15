package apicontract

const (
	// DaemonProtocolVersion identifies the version-independent discovery and
	// compatibility semantics implemented by this daemon.
	DaemonProtocolVersion = 1
	// PreferredAPIVersion is the daemon API major selected when a client has no
	// stronger preference.
	PreferredAPIVersion = "v1"
)

// ProtocolIdentity is the stable compatibility identity shared by every
// discovery document.
type ProtocolIdentity struct {
	DaemonProtocolVersion int    `json:"daemonProtocolVersion"`
	DaemonInstanceID      string `json:"daemonInstanceId"`
	DaemonBootID          string `json:"daemonBootId"`
	PreferredAPIVersion   string `json:"preferredApiVersion"`
}

// ProtocolSummary adds the current contract validators used by discovery,
// health, and readiness.
type ProtocolSummary struct {
	ProtocolIdentity
	OpenAPISHA256    string `json:"openapiSha256"`
	CapabilitiesETag string `json:"capabilitiesEtag"`
}

// DiscoveryDocument is the stable, version-independent bootstrap response.
type DiscoveryDocument struct {
	ProtocolSummary
	Product        string                  `json:"product"`
	DaemonVersion  string                  `json:"daemonVersion"`
	DaemonCommit   string                  `json:"daemonCommit,omitempty"`
	Authentication string                  `json:"authentication"`
	APIVersions    []string                `json:"apiVersions"`
	Links          DiscoveryLinks          `json:"links"`
	APIs           map[string]APIDiscovery `json:"apis"`
}

// DiscoveryLinks are version-independent daemon resources.
type DiscoveryLinks struct {
	Instance DiscoveryLink `json:"instance"`
}

// APIDiscovery identifies the contract resources for one daemon API major.
type APIDiscovery struct {
	OpenAPI      DiscoveryLink `json:"openapi"`
	Capabilities DiscoveryLink `json:"capabilities"`
	Health       DiscoveryLink `json:"health"`
	Readiness    DiscoveryLink `json:"readiness"`
}

// DiscoveryLink is a same-origin resource reference.
type DiscoveryLink struct {
	Href   string `json:"href"`
	SHA256 string `json:"sha256,omitempty"`
	ETag   string `json:"etag,omitempty"`
}

// CapabilityDocument reports the contract and deployment-specific route
// availability for one daemon.
type CapabilityDocument struct {
	ProtocolIdentity
	APIVersion    string            `json:"apiVersion"`
	SchemaVersion int               `json:"schemaVersion"`
	OpenAPISHA256 string            `json:"openapiSha256"`
	Routes        []RouteCapability `json:"routes"`
}

// RouteCapability describes one operation Fleet may invoke.
type RouteCapability struct {
	ID           RouteID      `json:"id"`
	Method       string       `json:"method"`
	Path         string       `json:"path"`
	ActionClass  ActionClass  `json:"actionClass"`
	Capability   CapabilityID `json:"capability,omitempty"`
	RequiredRole string       `json:"requiredRole"`
	Remote       bool         `json:"remoteInvocable"`
	Available    bool         `json:"available"`
	Code         string       `json:"code,omitempty"`
	Reason       string       `json:"reason,omitempty"`
	Streaming    bool         `json:"streaming"`
	RecoverySafe bool         `json:"recoverySafe"`
}
