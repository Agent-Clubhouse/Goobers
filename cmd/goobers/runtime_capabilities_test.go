package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
)

func TestRuntimeMutationCapabilityParity(t *testing.T) {
	uiCapabilities, err := loadUIRuntimeMutationCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	registries := []apicontract.SurfaceRegistry{
		{Surface: apicontract.SurfaceCLI, Capabilities: cliRuntimeMutationCapabilities()},
		{Surface: apicontract.SurfaceAPI, Capabilities: httpapi.RuntimeMutationCapabilities()},
		{Surface: apicontract.SurfaceUI, Capabilities: uiCapabilities},
	}
	if err := apicontract.ValidateRuntimeParity(apicontract.V1RuntimeCapabilities(), registries); err != nil {
		t.Fatal(err)
	}
}

func loadUIRuntimeMutationCapabilities() ([]apicontract.CapabilityID, error) {
	path := filepath.Join("..", "..", "portal", "src", "api", "runtimeCapabilities.json")
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var registry map[apicontract.CapabilityID]bool
	if err := json.Unmarshal(content, &registry); err != nil {
		return nil, err
	}
	capabilities := make([]apicontract.CapabilityID, 0, len(registry))
	for capability, registered := range registry {
		if !registered {
			return nil, fmt.Errorf("UI runtime capability %q must be registered as true", capability)
		}
		capabilities = append(capabilities, capability)
	}
	slices.Sort(capabilities)
	return capabilities, nil
}
