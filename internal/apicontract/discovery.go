package apicontract

// DiscoveryDocument is the stable, version-independent bootstrap response.
type DiscoveryDocument struct {
	Product             string   `json:"product"`
	DaemonVersion       string   `json:"daemonVersion"`
	DaemonCommit        string   `json:"daemonCommit,omitempty"`
	Authentication      string   `json:"authentication"`
	PreferredAPIVersion string   `json:"preferredApiVersion"`
	APIVersions         []string `json:"apiVersions"`
	OpenAPI             string   `json:"openapi"`
	Capabilities        string   `json:"capabilities"`
	Instance            string   `json:"instance"`
	Health              string   `json:"health"`
	OpenAPISHA256       string   `json:"openapiSha256"`
}

// CapabilityDocument reports the contract and deployment-specific route
// availability for one daemon.
type CapabilityDocument struct {
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
	Available    bool         `json:"available"`
	Reason       string       `json:"reason,omitempty"`
	Streaming    bool         `json:"streaming"`
	RecoverySafe bool         `json:"recoverySafe"`
}
