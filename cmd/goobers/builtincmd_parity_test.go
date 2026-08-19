package main

import (
	"testing"

	"github.com/goobers/goobers/internal/builtincmd"
	"github.com/goobers/goobers/internal/providerstage"
)

// cliRegistryNames returns every name the top-level CLI dispatcher resolves —
// each registered command's canonical name plus its aliases. Only top-level
// names count: a workflow run.command invokes ["goobers", "<name>", ...], so
// <name> must dispatch at the top level.
func cliRegistryNames() map[string]bool {
	names := make(map[string]bool)
	for _, command := range cliCommands {
		for _, name := range command.names {
			names[name] = true
		}
	}
	return names
}

// TestBuiltincmdInventoryDefersToCLIRegistry pins the deference direction:
// cliCommands is this repo's declared source of truth for the command surface
// (#1095, CLI-1), and internal/builtincmd is a data-only projection of the
// subset workflows may shell out to. Every inventoried name must resolve in
// the registry, so the inventory can never invent a command the binary does
// not actually have — the failure mode the inventory exists to reject.
func TestBuiltincmdInventoryDefersToCLIRegistry(t *testing.T) {
	registered := cliRegistryNames()
	for _, name := range builtincmd.Names() {
		if !registered[name] {
			t.Errorf("builtincmd inventories %q but the CLI registry has no such command — remove it from internal/builtincmd (the inventory defers to cliCommands, never forks it)", name)
		}
	}
}

// TestProviderStageManifestIsInBuiltincmdInventory pins the other containment:
// every command internal/providerstage describes is by definition a built-in
// a workflow may invoke, so the inventory must not lag the manifest.
func TestProviderStageManifestIsInBuiltincmdInventory(t *testing.T) {
	for _, name := range providerstage.Commands() {
		if !builtincmd.Known(name) {
			t.Errorf("providerstage manifest describes %q but internal/builtincmd does not inventory it — add it (the inventory must be a superset of the manifest)", name)
		}
	}
}
