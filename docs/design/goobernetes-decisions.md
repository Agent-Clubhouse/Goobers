# Goobernetes v1 — decision record

Status: **approved** (PO, 2026-08-22). This file is the authoritative record of the product-owner rulings
that scope Goobernetes v1 — the distributed "mode 3" execution model. The six Goobernetes design
documents encode these decisions; where a design document and this record disagree, this record wins
until amended by the PO. Each decision that overturns a prior ruling names it.

## D0 — Posture and contracts

- **The product is the DSL and the output** (the run journal, the PRs, the provider effects) — not the
  implementation. Implementations are revisitable as long as those contracts hold.
- **The only backward-compatibility contract is DSL 2.0 on the local runner.** DSL 3.0 also fully
  supports local; 2.0 simply never learns distributed features. 2.0 will eventually EOL (not now).
  DSL 1.4 (already deprecated) is **dropped**.
- **Zero deference to the shipped cloud topology.** Continuity for the existing cloud deployment is a
  non-goal. Shipped tier-3 machinery (projection loop, worker wiring, queue naming) carries forward
  only where it is the right design, never for its own sake.
- **Temporal is retained** — proven, fits the shape — but held loosely as an implementation detail
  behind clean contracts (see D5: the journal is decoupled from Temporal history). If it ever fights
  the shape, it is revisitable without touching the product surface.

## D1 — Modes

Three modes, one DSL, one repo:

1. **Local** — unchanged. macOS/Linux/Windows hosts; macOS is *mode-1 only* (Azure offers no macOS
   substrate; an `os: macOS` stage on a cloud instance fails validation, which is the system working
   as intended). Nothing may break Mac local.
2. **Cloud single-pod** — the existing daemon-in-a-pod shape. Issue #3482 (move execution out of the
   API pod) is **increment 1 of Goobernetes**, not a separate design.
3. **Goobernetes (distributed)** — every stage attempt executes in its own fresh, never-reused
   container pod, disposed after its outputs are surrendered. Windows stages run as **container pods**
   in the cluster (resolves #651 in the container direction, on the spike-ladder evidence; the
   provisional persistent-VM posture in mixed-platform-cloud-nodes.md §3 is superseded).

## D2 — DSL 3.0

- **Clean break**, shipped with a migrator (`goobers fix --to 3.0`). The binary carries two
  interpreters: 2.0 (frozen) and 3.0.
- **Naming.** Credential grants keep the name `capabilities:` unchanged (closed registry, security
  surface untouched). The scheduling surface is a new stage-level block:

  ```yaml
  tasks:
    build:
      capabilities: [repo:push]          # credential grants — unchanged
      runsOn:
        os: linux                        # validated enum: linux | windows | macOS
        cpu: 2000m                       # k8s quantity strings, verbatim
        memory: 4Gi
        disk: 20Gi
        capabilities: [go@1.26, make, gcc]   # open tag set
  ```

  `requiredCapabilities` is migrated to `runsOn.capabilities`; `os=<goos>` tokens are migrated to
  `runsOn.os` and **bare `os=*` tokens are rejected in 3.0** — this formally supersedes the locked
  #659 ruling ("platform is a token, not a field"); the drift hazard that ruling guarded against is
  closed by making the two vocabularies unable to coexist in one document.
- **Quantities** use Kubernetes quantity strings verbatim (`2000m`, `4Gi`). Declared minimums become
  pod resource *requests*; *limits* come from the runner's declared ceiling, never from the stage.
  On local modes, resource requirements are advisory (warnings), never errors.
- **Explicit-complete semantics.** Unspecified = no requirement; authors never write "any". An
  os-unspecified stage has no OS requirement: placement policy (ours to own) prefers — and will wait
  for — a Linux-class runner when the inventory has one, and on a single-OS instance the stage runs
  wherever it can.
- **Base contract + derivation.** Every runner implicitly provides the *base runner contract*
  (goobers binary, stage-contract environment, network to provider endpoints, credential delivery) —
  it is not a declarable tag. Tier-1/2 authors write nothing. Derived requirements keep annotation
  burden near zero: builtins derive their needs from stage identity via the providerstage manifest
  (which **gains DSL-version linkage as a funded prerequisite** — it is unversioned today);
  agentic stages derive `harness:<name>` from the goober's existing `harness:` field (declared once,
  never re-typed per stage — and it is what lets harness-less runner images exist); `sh`/`make`
  stages derive `shell`. Credentials remain strictly capability-gated: nothing is materialized
  without a declared credential capability.
- **Preview features:** the per-gaggle sandbox override folds into the restrictions model (D7);
  external call-out promotes to 3.0 GA. Both stay frozen preview in 2.0.

## D3 — Instance: runners inventory

- **`runners:` (plural) supersedes `runner:` (singular).** An instance with no `runners:` treats the
  legacy singular block as the implicit `self` entry — zero-change upgrade for every local install.
  Instance config gains a schema-version field as part of this change. Inventory changes are
  **restart-only in v1** (hot-apply deferred).
- Runner entry shape: `name`, `host` (self | image ref | deployment name; the kind set is designed
  extensible), `provides:` (os, cpu, memory, disk, capabilities), `restrictions:` (D7). An uber-runner
  providing the superset is legal and is the expected simple configuration.
- **`host: deployment` names a consumer-authored pod template by reference**: the dispatcher
  instantiates a fresh pod per stage attempt from the named Deployment's template. The
  fresh/never-reused lifecycle (D1) holds for **every** host kind — resident stage-executing
  workers are not part of v1.
- **`engine:` stays optional connection config** (Temporal hostPort/namespace/taskQueue); mode is
  inferred from inventory shape. A runner with a non-self host and no `engine:` is a validation error.
- **Runner claims are trusted in v1** (the RRQ-1 model): a false claim degrades to a runtime error
  with a named diagnostic. Probe-pod verification is a later honesty layer. The design docs state
  this trust model explicitly.

## D4 — Admission: three checkpoints

1. **Apply/validate:** full per-stage constraint solve (stages × runners). **Error** when a
   `runners:` inventory is declared (fixing the #3497 severity trap for the declared case); warning
   otherwise.
2. **Dispatch:** a bounded fail-fast (schedule-to-start class) remains the reality backstop —
   apply-time truth rots (spot eviction, scaled-to-zero pools). Capability-unsatisfiable is an
   apply-time error; capacity-exhausted is a bounded, named runtime failure.
3. **Boot: never kills the daemon.** #2860's decided-but-unimplemented ruling ("refusing one run is
   proportionate; refusing to start is not") is implemented as part of this work.

Inventory updates follow **accept-and-pin**: in-flight runs finish against their pinned snapshot
(this also answers the #3449 ruling: no drain machinery is required for config updates).

## D5 — State, journal, and the daemon write API

- **The daemon is the control plane.** Claims, provider quota, open-PR caps, and fairness stay
  daemon-owned. The tier-3 scheduler fork (`internal/scheduler`) is deleted; `cmd/goober-runtime`
  is retired (#2055 resolved: supersede).
- **Daemon write API** (v1 surface): claim/release for ledger-touching stages, external trigger
  ingestion, HITL escalation resolution — co-designed with the claims store per
  `distributed-state-and-coordination.md` (promoted from the spike branch; "designing them separately
  produces three coordinators"). This removes the kubectl-exec-only operation of distributed runs.
- **Live journal service.** The write API also carries the run journal: stage activities emit journal
  events as they happen (idempotency keys for retried activities); one writer service owns sequence
  allocation. The journal — the product output — is thereby **decoupled from Temporal history**, and
  runs are live: stall detection, SSE, and the portal work mid-run. Live **stage-transition
  visibility is a v1 functional requirement** (per-token streaming is not).
- Placement provenance (runner name, node, OS, image, attempt) is recorded as journal events under
  the `runner.*` namespace (non-conformance-normative) and surfaced via new StageAttempt read-model
  fields.

## D6 — Data planes

- **Repo state: declared handoffs, provider-remote transport.** 3.0 makes the silent worktree chain
  inexpressible: a repo-writing stage declares its handoff edge, and the compiler rejects the
  undeclared chain (generalizing #2861 from cross-OS to cross-runner). At a declared boundary in
  mode 3, the runtime pushes the run branch (`goobers/<workflow>/<run-id>`) to origin and the next
  stage's fresh worktree fetches it. Modes 1/2 are unchanged (same host, no push). A cluster-internal
  git remote is a deferred optimization behind the same contract.
- **Artifacts:** the content-addressed blobstore plane continues (write-through before pod disposal;
  materialize-before-stage).
- **Eviction: reachability-keyed GC.** A digest is evictable only past the adoption watermark AND
  when unreferenced by journal artifact pointers; conservative TTL baseline in v1. Run completion is
  an eligibility signal, never the trigger by itself.
- **Caches:** the warm-cache economics (Go build cache et al.) are acknowledged as deferred
  optimization — but stage `timeoutSeconds` defaults are resized for cold paths as a *functional*
  v1 item, so cold execution degrades to slow, never to spurious timeout failures.

## D7 — Restrictions

- **Effect-based closed list**, v1: `network:none`, `network:allowlist`, `fs:readonly-except-workspace`,
  `tmp:ephemeral`, `env:default-deny`. Restrictions name effects, never mechanisms; seccomp/Squid/LPAC
  appear only as implementation examples.
- **One model, three inputs:** restrictions are runner *properties*; a stage may *require* a
  restricted runner via `runsOn`; the instance posture is a *mandate* (an instance owner in a shared
  instance can impose "agentic stages get an egress allowlist" on gaggle authors who never asked).
  Strengthen-only (SEC-021) is preserved; conflicts are unsatisfiable at apply — no schedule.
- **Linux-pod-only enforcement in v1.** `network:allowlist` on Linux is CIDR-NetworkPolicy-backed per
  the standing #2898 PO ruling (2026-08-16); the proxy graduates later as the audit/FQDN layer
  (#1307). A stage requiring a restriction can only match runners that enforce it. **Windows
  sandboxing gets its own epic** — nice-to-have local, eventual (not distant) must-have for cloud.
- Pod-level restrictions are applied by the pod creator (the dispatcher); network-level restrictions
  ship as rendered per-runner-class reference manifests verified by doctor (plus #3301's
  rendered-together CI assertion).

## D8 — Images

- **Product-owned minimal images, consumer-owned fat layers.** Published official images (the "big
  if" resolved as a minimal set): `goobers-base` (binary + certs + git; near-distroless), and
  `goobers-harness-copilot` / `goobers-harness-claude`. Targets: **Azure Linux and Windows Server
  2022** only. Everything fatter (toolchains, CI stacks) is layered by consumers atop a documented
  image/layering contract. Publishing rides the existing release engine; **image tag = binary
  version** is the version-skew contract. Publishing an official Windows image explicitly promotes
  windows/amd64 from TierExperimental — stated, not implied. (Scopes #3275.)
- The minimal base image is **v1-functional, not optimization**: it is what "simple stages route to
  tiny fast Linux runners" schedules onto. Per-toolchain image atomization is the deferred tail.

## D9 — Scheduling and pools

- Task queues key on **(gaggle × runner-type)** — #656's fairness/isolation contract survives.
- **Warm pools are deferred** (optimization; cold-start correctness first). Recorded constraint for
  that later design: pools must be shareable across gaggles per runner type — forecast demand
  per-gaggle, throttle/account per-gaggle, but back low-frequency gaggles from one shared pool
  rather than N idle singletons (supersedes #666's per-gaggle pool keying). Open point recorded:
  what identity/credentials a pre-warmed shared pod may hold before a gaggle claims it.
- Windows placement facts are solver rules: ledger-touching stages never place on Windows (they
  structurally cannot reach instance state); Windows dispatch timeouts default higher to absorb
  scale-from-zero node provisioning and multi-GB pulls, with diagnostics that name the cause.

## D10 — Portal and telemetry

- Internet-reachable portal behind the customer's OIDC issuer (Entra as configuration, never a code
  path). **Frontend OIDC flow** (SPA acquires tokens, sends Bearer; the daemon-side authenticator is
  already shipped). Folds #2901 + #644; the #640 fail-closed-loopback reversal gets its own decision
  record. Temporal/trace navigation rides #2912 (run ID = trace ID). Telemetry stays vendor-neutral
  OTLP; App Insights is a collector destination.

## D11 — v1 exit: distributed-shape smoke (not a scale test)

~2 Linux + 2 Windows nodes, ~6 pods total. Proves: per-stage fresh pods; OS hopping; declared-edge
data flow (repo + artifacts) across nodes; repass across nodes with a fresh runner; pod-kill and
node-kill during each stage class classified as infra attempts; triggers + HITL through the write
API; live stage-transition visibility. **The volume scale test is deferred**; its candidate targets
(~25 concurrent runs, 24h soak, dispatch-latency ceilings) are captured in the smoke doc as the
explicit next rung, gated on the #2871 parity items (notably #2875 cumulative budgets) so a real
scale run cannot burn spend invisibly.

## D12 — Issue strategy

Epic **#2889 remains the umbrella**, re-scoped to this design. Overlapping issues are folded in,
closed as superseded (citing the decision), or explicitly deferred — never re-filed: #1087, #1529,
#1494 (per-task `run.image` stays rejected; runner-level images supersede), #2860 (implement), #2861
(generalize), #2898/#1307 (per D7), #2901/#644, #2912, #2953, #3275 (scoped per D8), #3276, #3482
(increment 1), #2051/#2053 (deferred, rationale recorded), #2055 (delete), #656 (encoded in D9),
#662/#666 (deferred warm-pool family, D9 constraint recorded), #2871's open parity gaps (pre-scale
gate). New sub-epics: Windows sandboxing; deferred warm pools; deferred volume scale.
