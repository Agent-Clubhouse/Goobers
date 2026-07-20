package apicontract

// Invalidation identifies versioned read models that clients should refetch.
type Invalidation struct {
	Cursor    string        `json:"cursor"`
	Models    []string      `json:"models"`
	RunIDs    []string      `json:"runIds,omitempty"`
	Workflows []WorkflowRef `json:"workflows,omitempty"`
}

// WorkflowRef identifies one workflow read model.
type WorkflowRef struct {
	Gaggle string `json:"gaggle,omitempty"`
	Name   string `json:"name"`
}

// ErrorEnvelope is the single error shape returned by every API route.
type ErrorEnvelope struct {
	Error APIError `json:"error"`
}

// APIError is a stable machine code and safe human-readable message.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
