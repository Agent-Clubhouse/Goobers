package apicontract

import "github.com/goobers/goobers/internal/apicontract/basewire"

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
type ErrorEnvelope = basewire.ErrorEnvelope

// APIError is a stable machine code and safe human-readable message.
type APIError = basewire.APIError
