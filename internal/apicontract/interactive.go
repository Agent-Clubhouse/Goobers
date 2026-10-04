package apicontract

// InteractiveCapabilities distinguishes configured authority from currently
// implemented operations. It never contains secret values or secret references.
type InteractiveCapabilities struct {
	Gaggle           string                        `json:"gaggle"`
	PolicyConfigured bool                          `json:"policyConfigured"`
	Viewer           bool                          `json:"viewer"`
	Operator         bool                          `json:"operator"`
	SourceWriteMode  string                        `json:"sourceWriteMode"`
	Actions          []InteractiveActionPermission `json:"actions"`
}

// InteractiveActionPermission reports why an action is disabled. Authorized
// does not imply a credential exists or an operation has been implemented.
type InteractiveActionPermission struct {
	Action               string `json:"action"`
	Authorized           bool   `json:"authorized"`
	CredentialConfigured bool   `json:"credentialConfigured"`
	Available            bool   `json:"available"`
	ReasonCode           string `json:"reasonCode"`
}
