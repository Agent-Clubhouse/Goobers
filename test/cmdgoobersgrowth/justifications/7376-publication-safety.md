# Issue 7376: publication safety admission

This PR adds 86 non-test Go lines and one direct file to `cmd/goobers`.
The command layer must check the exact outgoing commit before push, remediation
push, and PR creation, and verify that the provider sees the inspected commit
before opening a PR. These checks belong at those existing command boundaries,
where the selected base, local checkout, hardened Git environment, provider,
and publication credentials are available together.

The new `publicationsafety.go` adapter connects the command package's existing
`workspaceGitCommand` hardening to bounded, cancellable local Git reads. It also
connects startup diagnostic cleanup to the existing journal error reporter.
Checkout creation installs exclusions, and the publication commands propagate
the inspected commit to the existing push and provider operations. Keeping these
small adapters next to their callers preserves the command-specific environment
and error handling without exposing CLI internals to reusable packages.

Reusable policy and mechanics live outside the command package:
`internal/publicationguard` scans outgoing history and credential-shaped blobs,
`internal/publicationpaths` owns reserved paths and workspace exclusions, and
`internal/diagnostictmp` owns private diagnostic scopes and their lease-based
cleanup. Child publication uses the same guard. The PR also extracts existing
open-PR setup into a helper, so the net command growth is limited to boundary
integration rather than a second implementation of the scanning policy.

The shared informational growth baseline is unchanged. Native regression tests
cover rejected artifacts and credentials, incomplete and merged history, exact
remote-tip admission, safe source changes, and cleanup of inherited artifacts.
