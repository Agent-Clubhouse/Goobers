package main

import (
	"fmt"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

// pinnedClaimVisibility reads the trusted, digest-verified start-time workflow.
// It must never substitute the latest config or a stage-provided mode. Provider
// support is checked before shared coordination can mutate either ledger.
func pinnedClaimVisibility(reader *journal.Reader, identity journal.RunIdentity, provider providers.ProviderKind) (string, error) {
	machine, err := runner.PinnedWorkflowMachine(reader, identity)
	if err != nil {
		return "", fmt.Errorf("claim visibility requires a verified workflow pin: %w", err)
	}
	if machine.Def.Spec.Gaggle != identity.Gaggle {
		return "", fmt.Errorf("claim workflow pin belongs to another gaggle")
	}
	mode := machine.Def.Spec.Readiness.ClaimVisibility
	switch mode {
	case "", "local":
		return "local", nil
	case "shared":
		if provider != providers.ProviderGitHub {
			return "", fmt.Errorf("shared claims are unsupported for provider %q", provider)
		}
		return "shared", nil
	default:
		return "", fmt.Errorf("unsupported pinned claim visibility %q", mode)
	}
}
