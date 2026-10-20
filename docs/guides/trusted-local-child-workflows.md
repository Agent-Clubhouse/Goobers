# Trusted-local child workflows (preview)

Trusted-local is the daemon's child execution provider for an instance whose
resolved runners are all `self` and whose engine projection is disabled. It
requires no additional YAML flag. A distributed runner that is unavailable does
not enable host fallback. The parent task still declares DSL 3.1
`childWorkflows`, its allowed Goobers and capability ceiling, and the preview
annotation.

Implementation qualification is in progress; daemon startup activation remains
disconnected until the production lifecycle gates pass.

## Execution contract

The normal local harness and deterministic executors run each physical attempt
in a fresh checkout, reconstructed from the retained snapshot. They retain the
configured OS sandbox wrapper. A shared permit serializes child invocations;
waiting parents release their normal scheduler capacity. Self-execution denial,
required host capabilities and the existing instance/workflow limits still apply.

Local and distributed children retain the same accepted-source identity,
`TerminalResultInput`, result snapshots, parent wait/continuation, and
merge/replace/discard operations. The same journal events drive Portal history
and receipts. Local attempts record self placement and physical writer custody;
they do not fabricate pod dispatch or surrender events.

The daemon imports output only after `internal/invoke` receives proof that every
registered workspace writer has stopped. Cancellation is not that proof. An
uncertain process tree or a daemon crash with an unjoined writer leaves custody
retained and blocks replacement or resume. A reusable PID cannot establish safe
recovery. A new attempt never adopts an uncertain checkout.

## Authentication and security

This provider has the trust posture of ordinary local workflow execution under
the daemon's OS account. It does not isolate hostile code from other resources
accessible to that account. The configured sandbox remains in force, including
its existing platform limitations.

A child receives only a subset of the parent's credentials. In this version its
model credential must be the parent's exact selected grant/reference, or its
ambient authentication must be the same selected harness login. An explicit-key
parent cannot fall back to a stored login, and an ambient parent cannot acquire
an unrelated stored key. Codex keyring-only policy cannot widen to file-backed
credentials. A future profile-delegation extension requires explicit profile and
credential-reference declarations in the parent's DSL ceiling plus instance grants;
selecting another Goober alone grants no access to another secret.

Launches use a private home and an explicit environment. Goobers does not forward
unrelated provider tokens, Git helpers, agent sockets or other CLI logins. Config
discovery, wrapper handshakes and preflight must use the delegated scope or remain
offline. Canonical publication continues through its existing host owner; authored
shell commands do not receive publication credentials.

## Qualification

The hermetic journey uses real daemon coordination, queue, scheduler, local
executors, process ownership, snapshots and journals, with fake model and Git
origin adapters. It requires `git` and `sh`, no model service, Temporal or pods:

```sh
go test -race -timeout 30m -tags integration ./cmd/goobers \
  -run '^TestIntegrationTrustedLocalChildJourney$' -count=1
go test -race -timeout 30m -tags integration ./internal/childpod \
  -run '^TestIntegrationLocalExecutor' -count=1
```

Run these on macOS and Linux. Windows also requires a cross-GOOS build:

```sh
GOOS=windows GOARCH=amd64 make build
```

A cross-build does not qualify runtime writer custody. Windows local child
execution remains **unqualified preview** until the strict Job Object runtime
test passes on Windows. Missing Job custody or unobservable process accounting
must fail closed; a dead root process does not establish an empty process tree.
