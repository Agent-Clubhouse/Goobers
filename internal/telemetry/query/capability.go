// Package query exposes the telemetry rollup to the two consumers issue #24
// names: an operator (via a future CLI, #23) and a capability-gated agentic
// stage (via a future runner/executor, #19). It sits above
// internal/telemetry/rollup (the storage/aggregate layer) and knows nothing
// about SQL; it only shapes rollup results into artifact bytes and enforces
// the one V0 access-control rule this issue asks for: an agentic stage must
// declare the telemetry:read capability to receive query results at all.
package query

import (
	"encoding/json"
	"errors"
	"fmt"
)

// CapabilityRead is the capability grant an agentic stage's definition must
// declare to receive telemetry query results (ARCHITECTURE.md §5: undeclared
// capability use fails closed). It matches the dotted "resource:verb" shape
// #12/#14 already use for provider/credential capabilities (e.g.
// "github:issues:write").
const CapabilityRead = "telemetry:read"

// ErrCapabilityDenied is returned when the requesting stage's declared
// capabilities (api/v1alpha1.InvocationEnvelope.Capabilities) do not include
// CapabilityRead. This package does not validate the full capability grammar
// or admission rules (that lives with the compiler/#10's invocation envelope
// and #14's credential seam) — it enforces exactly the one gate this issue's
// acceptance criterion names: fail closed without the grant.
var ErrCapabilityDenied = errors.New("telemetry: telemetry:read capability not granted")

// HasCapability reports whether capabilities grants grant.
func HasCapability(capabilities []string, grant string) bool {
	for _, c := range capabilities {
		if c == grant {
			return true
		}
	}
	return false
}

// MaterializeArtifact JSON-encodes result as the bytes for a pointer-addressed
// artifact — but only if capabilities grants CapabilityRead; otherwise it
// fails closed with ErrCapabilityDenied and produces no bytes.
//
// This function is artifact-store-agnostic: it does not import
// internal/journal or api/v1alpha1, matching #22's decoupling. The caller
// (once #19's agentic executor exists) is expected to pass the stage's real
// InvocationEnvelope.Capabilities and, on success, hand the returned bytes to
// journal.Run.RecordArtifact to obtain the ArtifactPointer the invocation
// envelope's ContextPointers carry.
func MaterializeArtifact(capabilities []string, result any) ([]byte, error) {
	if !HasCapability(capabilities, CapabilityRead) {
		return nil, ErrCapabilityDenied
	}
	b, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("telemetry: marshal query result artifact: %w", err)
	}
	return b, nil
}
