package httpapi

import "github.com/goobers/goobers/internal/apicontract"

// RuntimeMutationCapabilities returns capabilities registered by API mutation
// handlers. V1 has no mutation handlers.
func RuntimeMutationCapabilities() []apicontract.CapabilityID {
	return nil
}
