// Package basewire contains dependency-free transport primitives shared by
// low-level clients and the full daemon API catalog.
package basewire

// V1Prefix is the versioned daemon API root.
const V1Prefix = "/api/v1"

// RunRecoveryPath is the claim-authorized recovery transfer route.
const RunRecoveryPath = V1Prefix + "/runs/{run}/recovery"

// ErrorEnvelope is the single error shape returned by every API route.
type ErrorEnvelope struct {
	Error APIError `json:"error"`
}

// APIError is a stable machine code and safe human-readable message.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
