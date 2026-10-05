# HAW interactive restart execution

This PR adds the daemon composition for a human-authorized, pinned local restart.
The command package owns the existing configuration archive loader, harness
registry factory, runner registry, provider constructors, and shared worktree
manager. The added files connect those existing dependencies; they contain no
new command parser, HTTP route, or parallel credential store.

Reusable execution leases and policy reload cancellation live in
`internal/interactiveaccess`. Dedicated runner lifecycle enforcement lives in
`internal/runner`; process environment and per-run Git identity hooks live in
`internal/procenv`, `internal/harness`, and `internal/worktree`. The daemon-specific
composition remains here so it cannot become an alternate public authorization
API or accidentally import daemon configuration into those packages.

Supported execution is explicitly restricted to sandboxed API-key Claude Code
or Codex agents/reviewers and native GitHub/ADO CI polling. Shell/provider CLI
stages and credentialed external tools are refused pending identity-bound
implementations. Tests exercise archived source selection, lazy admission,
credential separation, revocation, and per-run/shared redaction.
