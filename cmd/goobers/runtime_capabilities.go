package main

import "github.com/goobers/goobers/internal/apicontract"

// cliRuntimeMutationCapabilities returns capabilities registered by CLI runtime
// mutation commands. V1 has no such commands.
func cliRuntimeMutationCapabilities() []apicontract.CapabilityID {
	return nil
}
