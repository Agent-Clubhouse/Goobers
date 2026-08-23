# Goobernetes deployment shape and the image contract

Status: approved — Goobernetes v1 design. Encodes the PO decision record in
goobernetes-decisions.md (2026-08-22).

This document owns two surfaces of Goobernetes v1 (mode 3, per decision record D1): the
**official image set and the layering contract** a valid runner image must satisfy
(record D8), and the **deployment shape** — how a `runners:` inventory entry (record D3)
maps onto cluster reality: dispatcher-created stage pods, consumer-authored pod
templates, and the node pools underneath them. It scopes #3275 (published images) and
records the disposition of #3276 (worker scaling signal).

Companions: goobernetes-decisions.md is authoritative throughout;
[k8s-infra-shape.md](k8s-infra-shape.md) supplies the cluster assumptions and namespace/
identity model this doc builds on; the sibling Goobernetes design docs own DSL 3.0
(record D2), the runners inventory and admission (D3/D4), state and the daemon write API
(D5), data planes (D6), restrictions (D7), and the smoke exit (D11). Where a shipped
artifact conflicts with this doc, this doc states the supersession explicitly.

**What this is not.** It is not cluster provisioning: the cluster is customer-procured
and customer-operated, and IaC stays out of scope exactly as the k8s-infra-shape banner
and epic #41 already rule — reference manifests plus doctor checks are the deliverable
shape, never Bicep/Terraform. It is not the warm-pool design (deferred, D9) and not the
per-toolchain image atomization tail (deferred, D8).

---

## 1. Decisions

Numbered DI-*n* (deployment/images) to stay distinct from the record's D-numbers; each
cites the record decision it implements.

| # | Decision | Why |
| --- | --- | --- |
| DI-1 | The published official image set is exactly three families: `goobers-base`, `goobers-harness-copilot`, `goobers-harness-claude` (record D8). Everything fatter is consumer-layered. | Resolves the "big if" as a minimal set. The base image is v1-functional, not optimization: it is what "simple stages route to tiny fast Linux runners" schedules onto. |
| DI-2 | Base targets are **Azure Linux** and **Windows Server 2022** only (record D8). The nanoserver-vs-servercore choice on Windows is **left to the layer** — the contract names required properties (§3), never a base variant. | Two substrates match the node-pool contract (§6). Pinning a Windows base variant would put the product in the business of every consumer's DLL surface; the contract properties are checkable, the variant is taste. |
| DI-3 | `goobers-base` is near-distroless: the `goobers` binary, CA certificates, git, tz data, and a POSIX-compatible shell — nothing else. Harness variants layer exactly one agent-harness runtime on top of base. | The shipped all-in-one image (packaging/docker/Dockerfile: Node 22 Debian base carrying goobers + operator + gh + Copilot CLI) is **superseded as the published shape** — it remains the ancestor and the single-image convenience for mode-2 reference deployments, but the published set separates control-plane from harness so deterministic stages never pay a Node runtime pull. tz data is on the required list because its absence in Windows base images broke the first OS-spanning run (#2863). The shell is on the required list, not an incidental base-image choice: a `runners:` entry built from `goobers-base` is meant to declare `provides.shell: true` (DI-14), and that declared claim is trusted (DI-11) — nothing re-verifies it against the image — so an unstated shell in a future true-distroless slim of `goobers-base` would make every such declaration silently false rather than fail loudly at config time. The infra build enforces the shell's presence directly (fails red if absent) precisely because the contract can't catch that failure mode on its own. |
| DI-4 | Non-root is contract, not convention: Linux images run as uid:gid **65532:65532** with a writable `HOME`; Windows images run as `ContainerUser`. Consumer layers must not raise privilege; the dispatcher sets `runAsNonRoot` and the pod fails admission if the layer broke it. | 65532 is already the repo-wide convention (packaging/docker/Dockerfile "mirrors the conventional distroless nonroot uid"; deploy/reference/goobers-system/api-deployment.yaml:27-29 and siblings pin `runAsNonRoot: true`, `runAsUser: 65532`). Windows has no uid concept and forbids privileged containers/DinD by platform (#2842) — `ContainerUser` is the equivalent posture. |
| DI-5 | The dispatcher invokes `goobers` **by name on `PATH` with dispatcher-chosen args**; a runner image's `ENTRYPOINT` is a convenience for humans, never part of the contract. | The pod spec's `command`/`args` are dispatcher-owned (it is also what applies restrictions, record D7). Contracting on ENTRYPOINT would let a consumer layer's wrapper script sit between the dispatcher and the stage contract. |
| DI-6 | **The version-skew contract compares embedded COMMITS** (delivery decision 003): the dispatcher launches stage pods only from an image whose embedded commit stamp equals its own. On a tagged release, tag = version = commit stamp, so "image tag = binary version" holds as the release-time reading; on continuous-main (the delivery phase — no tagged releases, per PO ruling), images are SHA-tagged (`goobers-base:<40-char-sha>`, never `latest` — #3452) and the same stamp satisfies the contract. | Skew between daemon and worker binaries is the recorded #1061 failure-mode class; commit-equality-by-construction is the cheapest correct answer that also survives continuous-main. Literal tag-string equality was measured to reject every image the live instance has ever built (infra finding, gbn-infra-02/C-1). A tolerated skew window is deliberately not designed in v1 (open point O-1). |
| DI-7 | Publishing rides the **existing release engine** (`go run ./release`, release/main.go): image build/push/sign become release targets emitted on tagged releases, alongside the binaries, from the same version/commit/date inputs. No second pipeline. | Today nothing publishes any image for any commit or platform (packaging/docker/Dockerfile: "nothing in .github/workflows builds or pushes it yet"; recorded as the #3278 entry-ticket gap). The release engine already owns versioning, checksums, and signing for binaries; images are one more target kind, which is what keeps DI-6 true by construction. Scopes #3275. |
| DI-8 | Publishing an official Windows Server 2022 image **explicitly promotes windows/amd64 from TierExperimental** in the SupportMatrix (internal/supportmatrix/supportmatrix.go) — a stated tier change staged as its own supportmatrix history entry, not an implication readers infer. | Record D8: "stated, not implied." The platform tier declaration is append-only and CI-checked against the latest tagged release, so the promotion must land as a deliberate entry, and the Windows image build path (#3275's hardest half — none exists today) is its prerequisite. |
| DI-9 | `runners:` host kinds map to cluster reality as: `self` → the daemon's own host (no cluster mapping); `image` → **dispatcher-created per-stage pods**, requests derived from `runsOn` quantities; `deployment` → a **consumer-authored pod template by reference**: the dispatcher instantiates one fresh pod per stage attempt from the named Deployment's template (sidecars, volumes, node selectors under consumer control). The fresh/never-reused lifecycle (record D1) holds for every host kind; a Deployment that also runs resident stage-executing replicas is not part of the contract. | Record D3's kind set as pinned in the record: the shipped resident `goobers worker` Deployment (deploy/reference/goobers-system/worker-deployment.yaml) is this shape's ancestor but is superseded as an execution substrate (architecture doc §10) — resident workers executing stages as goroutines reuse process state across attempts, which record D1 forbids. Template-by-reference keeps consumer pod-spec control without surrendering the lifecycle; per-stage pods are the A3 gap (#41) this design funds. |
| DI-10 | `deployment` runners are verified by **doctor-style advisory checks, never static-validate errors** — the named Deployment exists, its pod template satisfies the layering contract (§3), its image tag matches the daemon version. The strict/advisory line is reused verbatim from cmd/goobers/validatereality.go: config-shape findings are ordinary warnings (they count under `--strict`), cluster-reality findings print and land in the diagnostics envelope but never change the exit code. | Whether the named Deployment exists and what its template contains is cluster state that can change a minute later; validatereality.go already fought and settled this exact contract ("--strict must not fail a CI runner over it"). Statically erroring on unreachable cluster state would certify never-working configs on one side or red CI on the other. |
| DI-11 | Runner-image capability claims are **trusted in v1** (record D3, RRQ-1): nothing verifies that a runner's `provides:` is true of the image it names; a false claim degrades to a runtime error with a named diagnostic. Doctor gains an advisory image-inspection check; probe-pod verification is a later honesty layer. | The spike's workaround — "claim os=windows on a Linux container and document the lie" — proves the gap is real; the record rules the trust model explicitly rather than silently. |
| DI-12 | Node pools are **hand-managed by the customer**: a required Linux pool and an optional Windows pool carrying the `kubernetes.io/os=windows:NoSchedule` taint, exactly the deploy/reference/README.md contract. Karpenter/NAP stay deferred. Scale-from-zero is absorbed by dispatch, not by pools: Windows dispatch timeouts default higher with diagnostics that name the cause (record D9), and capacity-exhausted is a bounded, named runtime failure (record D4), never a hang. | Pool properties, not VM sizes, are the contract; AKS supplies the OS label but does not auto-taint Windows pools, which already caused a live incident (Windows node initializing a Linux volume, deploy/reference/README.md). |
| DI-13 | Per-task `run.image` **stays rejected** (record D12, #1494 closes as superseded): the only way a stage selects an image is by routing to a runner whose image satisfies it. | One selection surface. A task-level image field would bypass the runner inventory, the restrictions model (D7), and the admission solve (D4) in one stroke. |
| DI-14 | **Derived capability tags (`run:shell`, `harness:<name>`) are satisfied by a non-self runner through two new, closed, typed `provides:` fields — `shell` (bool) and `harnesses` ([]string) — trusted exactly like every other `provides:` claim (D10/DI-11), not by base-image identity/digest inference and not by reopening `provides.capabilities`** (#3513, resolving the open point `internal/runnercap` names; implemented in Goobers#3632). A runner declaring `provides.shell: true` satisfies a stage's derived `run:shell`; one declaring `provides.harnesses: [copilot]` satisfies `harness:copilot`. The author-token grammar stays closed to the derived colon-namespace exactly as it is today — these are separate fields, not new spellable tokens. | **Supersedes this row's original text** (base-image-digest inference, no config surface), which turned out to conflict with two decisions already on record once someone tried to build it: DI-2 ("the contract constrains properties, not lineage") rules out lineage-based matching as the model this system uses; DI-11 already rules the v1 trust posture for `provides:` explicitly ("nothing verifies that a runner's `provides:` is true of the image it names; a false claim degrades to a runtime error with a named diagnostic") — treating that as a "problem" to design around, rather than the accepted v1 answer, was the original mistake. It was also not buildable today regardless: no image is published for any commit yet (#3275), and `goobers validate`/`runnersolve` deliberately never contact a registry, so there was no digest to check against without new infra. A declared claim is DI-11's existing model applied to the one namespace `provides.capabilities` structurally can't carry. **Residual is unchanged**: these are still trusted, unverified claims — a runner that declares `shell: true` on an image without one still places, then fails at exec, exactly DI-11's named failure mode, not a new one. |

---

## 2. Image taxonomy

Three published families, two OS targets, one layering contract:

```
goobers-base:<version>                    goobers-base-windows:<version>
  Azure Linux · near-distroless            Windows Server 2022
  goobers binary, CA certs, git, tzdata    goobers.exe, CA certs, git, tz data
        │                                        │
        ├─ goobers-harness-copilot:<version>     │  (Windows harness images: see O-4)
        ├─ goobers-harness-claude:<version>      │
        │    base + one harness CLI runtime      │
        ▼                                        ▼
  consumer-layered fat images (toolchains, CI stacks) — customer-built,
  customer-registried, referenced from runners: by the customer
```

- **`goobers-base`** is what deterministic builtins, `sh`/`make` stages with no toolchain
  requirements, and control-plane pods run on. Small and fast to pull is a *functional*
  property (record D8): it is the substrate of "simple stages route to tiny fast Linux
  runners." It carries no harness, which is exactly what the D2 derivation rule
  (`harness:<name>` derived from the goober's `harness:` field) makes safe — a
  harness-less image can never be matched to an agentic stage.
- **`goobers-harness-copilot` / `goobers-harness-claude`** add exactly one agent-harness
  CLI (and its runtime — Node for both today) atop base. The harness binary on `PATH` is
  what makes a `provides.harnesses: [<name>]` declaration on a runner built from that
  image true (DI-14) — the declaration, trusted like any other `provides:` claim, is
  what actually discharges the derived `harness:<name>` requirement.
- **Consumer fat images** layer toolchains (Go, dotnet, JDK, CI stacks) on any official
  image per §3. The product publishes none of these; per-toolchain official image
  atomization is the deferred tail (record D8).
- **Windows** images target Windows Server 2022 to match the node pool (§6). The
  official Windows base picks whichever Microsoft base variant satisfies §3 with the
  smallest pull; consumers re-basing on nanoserver or servercore is their call — the
  contract constrains properties, not lineage (DI-2). tz data must be present whatever
  the variant: Windows base images ship without it and that absence blocked the first
  OS-spanning run (#2863).

The shipped single image (packaging/docker/Dockerfile) also carries `goobers-operator`;
in the published set the operator rides `goobers-base` with the operator entrypoint
(deploy/reference/goobers-system/operator-deployment.yaml already selects the entrypoint
per-Deployment, so no separate operator image is needed).

## 3. The layering contract

A **valid runner image** — official or consumer-layered — must satisfy all of:

1. **Binary present and versioned.** A `goobers` binary on `PATH`, whose reported
   version equals the image tag (DI-6). Consumer layers inherit it from the official
   base and must not replace or shadow it.
2. **Runtime-version compatibility.** The image is dispatched only by a daemon of the
   same version (DI-6). Consumers rebuild their fat layers per release; the release
   engine's tag stream is the rebuild trigger. (Skew tolerance: open point O-1.)
3. **Base runner contract materials** (record D2): CA certificates for provider
   endpoints, git, tz data. Credential *delivery* is the dispatcher's job, never baked —
   an image containing a credential is invalid by definition (nothing is materialized
   without a declared credential capability, record D2; CT-1 keeps operator surface out
   of stage pods).
4. **Workspace mount expectations.** The image assumes nothing about workspace location:
   the dispatcher mounts a writable, initially-empty workspace volume at a
   dispatcher-chosen path and passes it per the stage contract (`GOOBERS_*` environment,
   including the stage-attempt-scoped `GOOBERS_TELEMETRY_DIR`). Images must not bake
   state under the workspace path, must tolerate ephemeral `/tmp`-class storage, and
   must remain runnable with a read-only root filesystem outside the workspace and
   `HOME` — that is the `fs:readonly-except-workspace` restriction (record D7) applied
   by the dispatcher, and a layer that scribbles outside those roots fails under it.
5. **Non-root posture** (DI-4): Linux 65532:65532 with writable `HOME`; Windows
   `ContainerUser`. No setuid helpers, no privilege escalation; the dispatcher pins
   `runAsNonRoot`/`allowPrivilegeEscalation: false` and Windows forbids privileged
   containers by platform (#2842).
6. **Entrypoint neutrality** (DI-5): whatever `ENTRYPOINT` the layer sets, the image
   must behave correctly when the pod spec overrides `command` to invoke `goobers`
   directly.
7. **Harness images additionally** carry the harness CLI on `PATH` such that the derived
   `harness:<name>` capability is true, and a writable harness home (the daemon-side
   ENOENT from a missing writable harness-home is a known failure shape,
   deploy/reference note at bbbea3e4).

What the contract deliberately does **not** require: a specific base lineage (DI-2), any
toolchain, any harness in `goobers-base`, or truthfulness verification of `provides:`
claims beyond DI-11's advisory checks.

## 4. Publishing and version skew

The release engine (release/main.go, #655) grows image targets. Per tagged release, for
each family × OS target with a build path:

- build from the committed Dockerfile source with the release's version/commit/date (the
  same ldflags inputs binaries get);
- tag `<registry>/<family>:<version>` — the tag **is** the binary version, no `latest`
  channel for runner use (a floating tag is precisely the unrecorded-drift failure the
  Dockerfile's pinned `GO_IMAGE` comment documents, #3452);
- sign and checksum through the same release machinery as binaries;
- publish the Windows Server 2022 images from the new windows/amd64 build path
  (#3275's missing half), landing the DI-8 tier promotion as its own SupportMatrix
  entry staged per the append-only history rules.

Disposition of **#3276** (the worker Deployment publishes no scaling signal, so node
floor = peak concurrency): dispatcher-created per-stage pods dissolve the problem for
every runner kind — pending pods *are* the scaling signal every cluster autoscaler
already understands, no Goobers-published metric needed, and with `deployment` resolved
as template-by-reference (DI-9) there is no resident stage-executing worker left to
publish a signal for. #3276 closes as superseded by pod-per-stage, citing this section.

## 5. `runners:` → cluster reality

| host kind | Who creates what | Placement inputs | Verification |
| --- | --- | --- | --- |
| `self` | Nothing — the daemon's own host/pod is the runner (implicit entry for every legacy `runner:` config, record D3) | n/a | Existing local admission |
| `image` | The **dispatcher creates one pod per stage attempt** from the named image, in the gaggle's namespace, and disposes it after outputs are surrendered (record D1) | Pod resource **requests** come from the stage's `runsOn` quantities verbatim (k8s quantity strings, record D2); **limits** come from the runner's declared `provides:` ceiling, never from the stage; `nodeSelector: kubernetes.io/os` from the runner's OS, plus the Windows toleration; restrictions (D7) applied by the dispatcher in the pod spec | Claims trusted (DI-11); apply-time constraint solve per record D4 checkpoint 1; bounded dispatch fail-fast per checkpoint 2 |
| `deployment` | The **customer authors a Deployment as a pod template** (typically `replicas: 0`); the **dispatcher instantiates one fresh pod per stage attempt** from that template and disposes it (record D1) | The template's pod spec (sidecars, volumes, node selectors) under customer control; requests/limits, restrictions, and credential delivery are still dispatcher-stamped onto every instantiated pod (record D2/D7); the runner entry's `provides:` is the claim admission solves against | **Advisory doctor checks only** (DI-10): the Deployment exists, its template satisfies the layering contract, image tag matches daemon version, node placement matches declared OS. Never a static-validate error |

A runner entry with a non-`self` host and no `engine:` block is a validation error
(record D3). Queue keying is (gaggle × runner-type), record D9.

The `image` row is where restrictions and credentials meet the pod: the dispatcher is
the single component that renders `runsOn` + runner `restrictions:` + instance mandate
into a pod spec (record D7 — pod-level restrictions applied by the pod creator), and
credential delivery to the pod follows the D5 write-API / stage-scoped direction rather
than the shipped full-instance-config-per-worker shape (cmd/goobers/workerwiring.go),
which does not survive into per-stage pods.

## 6. Node-pool contract

Verbatim continuity with deploy/reference/README.md, now normative for mode 3:

- **Linux pool (required):** standard `kubernetes.io/os=linux` label; every Goobers
  workload — operator, daemon, workers, Temporal components, and all Linux stage pods —
  pins `nodeSelector: kubernetes.io/os: linux`. No Goobers taint needed.
- **Windows pool (optional):** Windows Server 2022 nodes, `kubernetes.io/os=windows`
  label, and the operator-applied `kubernetes.io/os=windows:NoSchedule` taint. AKS does
  **not** auto-taint Windows pools; the reference docs record the live incident that
  omission causes. Windows stage pods select the label and tolerate the taint; nothing
  else tolerates it.
- **Scale-from-zero** is expected on the Windows pool (and permitted on Linux). The
  design absorbs it in dispatch, not pool management: Windows dispatch timeouts default
  higher to cover node provisioning plus multi-GB image pulls (record D9) — which is
  also why the Windows base image being as small as its variant allows (DI-2) is a
  cost lever, not cosmetics — and exhaustion surfaces as record D4's bounded, named
  runtime failure. Autoscaler choice (cluster autoscaler, Karpenter, NAP) is the
  customer's; the product's only demand is that pending pods eventually schedule or the
  bounded dispatch window expires with a diagnostic naming the pool.

## 7. Ownership split

| Customer-managed | Goobers-created and -owned |
| --- | --- |
| The cluster itself, upgrades, node pools, cost (k8s-infra-shape §1; IaC out of scope per its banner and #41) | Per-stage pods for `image` runners: created by the dispatcher, disposed after output surrender, never reused (record D1) |
| Registry, plus all consumer-layered fat images (§2) | Official images published per release (§4) |
| `deployment` pod templates: the Deployment manifest and the images its template names | Pod-spec rendering for stage pods (all host kinds): requests/limits, selectors, tolerations, restrictions, credential delivery (§5) |
| GitOps sync, cluster RBAC, OIDC issuer (k8s-infra-shape §2–§3) | Doctor/advisory verification of the seam (DI-10, DI-11) |

## 8. Smoke deployment reference shape

The record D11 smoke (distributed-shape proof, not a scale test) deploys on:

- **2 Linux nodes + 2 Windows Server 2022 nodes**, ~6 pods total, node pools per §6
  (Windows pool tainted, everything pinned by OS selector);
- control plane (daemon/API, operator, Temporal) on the Linux pool from `goobers-base`;
- stage pods dispatched from all three official families — `goobers-base` for
  deterministic/shell stages, a harness image for agentic stages — plus at least one
  consumer-layered image to exercise §3 as a contract rather than a courtesy;
- two nodes per OS is the minimum that makes "declared-edge data flow across nodes,"
  "repass across nodes with a fresh runner," and node-kill classification (D11's proof
  list) observable at all — one node per OS would let same-node luck mask a broken data
  plane.

The smoke doc (sibling) owns the proof matrix; this shape is its substrate.

## 9. Acceptance criteria (falsifiable)

1. `docker inspect` of every published image shows a `goobers` (or `goobers.exe`) on
   `PATH` whose embedded commit stamp (`goobers --version`) satisfies DI-6's
   commit-equality contract against the tag (release: tag = version; dev: tag = sha); a CI
   check runs the comparison per publish and fails on mismatch (DI-6, DI-7,
   decision 003).
2. `goobers-base` on Linux contains no shell-installed harness, no Node runtime, and no
   package manager cache; its compressed size is published per release and a regression
   above an agreed budget fails the release (DI-3 — the budget number is an
   implementation choice, its existence is not).
3. Every published image runs a trivial stage successfully under
   `runAsNonRoot: true` + `runAsUser: 65532` (Linux) / `ContainerUser` (Windows) with a
   read-only root filesystem outside workspace and `HOME` (DI-4, §3.4–5).
4. A tagged release publishes signed Linux **and** Windows images from the release
   engine with no hand steps, and the same release's SupportMatrix entry shows
   windows/amd64 out of TierExperimental (DI-7, DI-8; closes #3275's "no image for any
   commit or platform" and "no Windows build path").
5. A `runners:` entry with `host: image` produces a pod whose resource **requests**
   byte-match the stage's `runsOn` quantities and whose limits match the runner's
   declared ceiling; a stage exceeding a limit fails as that stage's policy failure,
   never as a daemon fault (§5, record D2).
6. A `runners:` entry with `host: deployment` naming a Deployment that does not exist
   yields: `goobers validate` exit 0 with an advisory finding, `goobers doctor` naming
   the missing Deployment, and — if dispatched anyway — record D4's bounded runtime
   failure naming the unserved queue. It never yields a static-validate error and never
   a hang (DI-10).
7. A workflow attempting `run.image` is rejected by schema, unchanged from today
   (DI-13; #1494 closed as superseded, not reopened).
8. On a cluster whose Windows pool is scaled to zero, a Windows stage either completes
   after node provisioning within the raised Windows dispatch timeout or fails with a
   diagnostic naming pool provisioning as the cause — never a generic timeout (§6,
   record D9).

## 10. Issue cross-references

- **#3275** — published, signed images: scoped by this doc (§4, DI-7/DI-8); the entry
  ticket for every adopter-facing Goobernetes claim.
- **#3276** — worker scaling signal: re-scoped to `deployment` runners only (§4);
  dissolved for `image` runners by pod-per-stage.
- **#41 / A3** — ephemeral per-stage pod execution: funded by this design (§5); its
  IaC-out-of-scope posture carries forward (§7).
- **#1494** — per-task `run.image`: closed as superseded (DI-13, record D12).
- **#1061** — version-skew failure class: answered by tag-equality (DI-6).
- **#2863** — tz data absent from Windows base images: baked into the contract (§3.3).
- **#2842** — Windows platform limits (no privileged/DinD, multi-GB pulls): encoded in
  DI-4 and §6.
- **#3452** — floating base-image tags: the no-`latest` rule (§4).
- **#3482** — execution out of the API pod is Goobernetes increment 1 (record D1); it
  lands on this doc's image set.

## 11. Open points

- **O-1 — skew window.** DI-6 pins v1 to tag equality. Whether a one-minor skew window
  is ever tolerated (for staged rebuilds of consumer-maintained `deployment` templates
  and fat images, where the customer controls rebuild timing) is deliberately undesigned;
  until decided, upgrading the daemon strands non-matching templates and images behind a
  doctor finding.
- **O-2 — registry layout and mirror guidance.** Which registry the official images
  publish to, and whether the docs recommend customers mirror them into their own
  registry (the k8s-infra-shape §1 assumption is only "a registry the cluster can
  pull from").
- **O-3 — base-image compressed-size budget.** Acceptance criterion 2 requires a
  number; picking it needs the first real Azure Linux build.
- **O-4 — Windows harness images.** D8 names harness variants without OS qualification.
  The harness itself is not the question — #647 closed 2026-08-11 (harness confirmed
  working on Windows; the spike's Windows agentic run merged a PR in 2m25s). What needs a
  ruling is publishing scope: whether `goobers-harness-*` build for Windows Server 2022
  in v1, or Windows agentic stages ride consumer-layered images until the
  Windows-sandboxing epic (record D7) takes the cell.
- **O-5 — doctor image-inspection depth.** DI-11's advisory check could range from
  "image pullable + tag matches" to pulling and inspecting for §3 properties; depth vs
  doctor runtime is an implementation trade.
- **O-6 — arm64.** The SupportMatrix supports linux/arm64 for binaries; whether official
  images publish multi-arch Linux manifests in v1 or amd64-only is unpinned.
- **O-7 — dispatcher identity for image pulls.** Which service account / pull secret
  stage pods use against the customer's registry for consumer-layered images, per-gaggle
  or shared — interacts with the per-gaggle workload-identity model (k8s-infra-shape §3)
  and the warm-pool identity open point recorded in D9.
