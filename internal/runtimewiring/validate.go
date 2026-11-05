package runtimewiring

import (
	"context"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
)

// CopilotAuthCheckArgs is the confirmed non-interactive Copilot authentication
// probe (#284/#271). The Copilot CLI has no auth-status subcommand, and
// `--version` succeeds even when signed out, so authentication is verified by a
// minimal, tool-disabled prompt: it exits 0 when the token is valid AND has the
// "Copilot Requests" fine-grained permission, and non-zero with an actionable
// auth error otherwise. `--available-tools=` (empty allowlist) disables every
// tool so the probe can never touch the filesystem or run shell commands;
// `--allow-all-tools` is still required to enable non-interactive mode.
// Configured forwarding launchers use a headless completion posture
// (`--silent --no-ask-user --autopilot`) and expose only `view` plus
// `task_complete`; no filesystem mutation or shell tool is exposed.
//
// This runs in BOTH the operator-invoked `goobers validate --check-harness` and
// the automatic daemon-startup preflight (adapterFor wires it into every
// CopilotAdapter, so preflightAgenticHarnesses picks it up too — #238). Direct
// Copilot and version-1 forwarding launchers use this model-request fallback;
// version-2 launchers can replace it with their declared non-agentic auth probe.
// The fallback runs once per process lifetime (once per `up` daemon boot, once
// per `run`), only for harnesses an agentic stage actually references.
var CopilotAuthCheckArgs = []string{"-p", "Reply with exactly: ok", "--allow-all-tools", "--available-tools="}

// CopilotAuthCheckSuccessLine is the reply copilotAuthCheckArgs asks for. The
// probe passes as soon as the CLI prints it, without waiting out the CLI's own
// shutdown, which on slow hosts took long enough after a valid reply to push
// the probe past harnessPreflightTimeout (#5165).
const CopilotAuthCheckSuccessLine = "ok"

// HarnessPreflightTimeout bounds a single harness preflight (its version check
// plus the auth probe's real API round-trip) so a hung CLI or network can't
// hang `goobers validate` or `goobers up`/`run` startup. One value serves all
// three so they cannot disagree about whether the same harness is healthy.
// It is deliberately generous: a valid Copilot probe was measured at 92-100s
// end to end on Windows (#5165), so the former 90s cap failed healthy hosts.
// The probe returns as soon as its reply arrives, so the cap only costs time
// when a CLI or network is genuinely hung.
const HarnessPreflightTimeout = 180 * time.Second

// HarnessAdapterFor is the harness-adapter lookup checkHarnessesAtSources uses.
// Package-level so tests can substitute a fake lookup without depending on a
// real, installed, signed-in Copilot CLI.
var HarnessAdapterFor = AdapterFor

// AdapterFor returns the registered adapter for a goober-declared harness kind,
// including the instance's configured ambient environment passthrough.
//
// The CopilotAdapter carries copilotAuthCheckArgs so every preflight — the
// operator-invoked `validate --check-harness` AND the automatic daemon-startup
// preflight (preflightAgenticHarnesses) — verifies sign-in, not just CLI
// presence (#238). Both look the harness up through here, so wiring the probe
// once here is what closes #238's "catch a signed-out harness at startup, not
// mid-run" criterion.
func AdapterFor(h apiv1.Harness, environment harness.EnvironmentConfig, harnessCommand map[string][]string, modelCredential func(ctx context.Context) (string, error)) (harness.Adapter, error) {
	registry, err := BuildHarnessRegistry(nil, environment, harnessCommand, "", "", false, modelCredential, false)
	if err != nil {
		return nil, err
	}
	return registry.Get(string(h))
}
