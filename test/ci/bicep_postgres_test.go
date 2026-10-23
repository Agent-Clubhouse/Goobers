package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestPostgresBicepIsZoneRedundantPrivateAndEntra compiles the reference
// offline and checks the PostgreSQL server the deployment would request
// (HA-P3-21): General Purpose, zone-redundant, VNet-only, Entra-enabled, with
// the instance and Temporal databases. It skips without a Bicep CLI.
func TestPostgresBicepIsZoneRedundantPrivateAndEntra(t *testing.T) {
	t.Parallel()
	binary := offlineBicepCLI(t)
	root := moduleRoot(t)
	command := exec.Command(binary, "build", filepath.Join(root, "infra", "bicep", "modules", "postgres.bicep"), "--stdout")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatalf("compile postgres.bicep: %v", err)
	}
	var template struct {
		Resources []struct {
			Type       string         `json:"type"`
			Name       string         `json:"name"`
			Copy       map[string]any `json:"copy"`
			SKU        map[string]any `json:"sku"`
			Properties map[string]any `json:"properties"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(output, &template); err != nil {
		t.Fatalf("decode compiled template: %v", err)
	}
	byType := make(map[string]int)
	for i, resource := range template.Resources {
		byType[resource.Type] = i
	}
	index, ok := byType["Microsoft.DBforPostgreSQL/flexibleServers"]
	if !ok {
		t.Fatal("compiled template has no flexible server")
	}
	server := template.Resources[index]
	properties := server.Properties
	for name, got := range map[string]any{
		"sku tier":              server.SKU["tier"],
		"high availability":     properties["highAvailability"].(map[string]any)["mode"],
		"public network access": properties["network"].(map[string]any)["publicNetworkAccess"],
		"Entra authentication":  properties["authConfig"].(map[string]any)["activeDirectoryAuth"],
		"storage auto-grow":     properties["storage"].(map[string]any)["autoGrow"],
	} {
		want := map[string]any{
			"sku tier": "[parameters('skuTier')]", "high availability": "ZoneRedundant",
			"public network access": "Disabled", "Entra authentication": "Enabled", "storage auto-grow": "Enabled",
		}[name]
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if _, ok := properties["network"].(map[string]any)["delegatedSubnetResourceId"]; !ok {
		t.Error("server is not VNet-integrated")
	}
	if _, ok := byType["Microsoft.DBforPostgreSQL/flexibleServers/firewallRules"]; ok {
		t.Error("compiled template opens a firewall rule on a private server")
	}
	if index, ok := byType["Microsoft.DBforPostgreSQL/flexibleServers/databases"]; !ok || template.Resources[index].Copy == nil {
		t.Error("compiled template does not create the database set")
	}
}

func offlineBicepCLI(t *testing.T) string {
	t.Helper()
	if binary, err := exec.LookPath("bicep"); err == nil {
		return binary
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("Bicep CLI is not installed")
	}
	binary := filepath.Join(home, ".azure", "bin", "bicep")
	if info, err := os.Stat(binary); err != nil || info.IsDir() {
		t.Skip("Bicep CLI is not installed offline")
	}
	return binary
}
