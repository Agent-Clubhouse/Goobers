# Goobernetes dispatcher — the pod-per-stage substrate (infra-facing design)

Status: Draft — Goobernetes v1 design. Encodes the PO decision record in
[goobernetes-decisions.md](goobernetes-decisions.md) and delivery decisions 003/006/007/009.
This document resolves the dispatcher's cluster-facing surface so the infra collateral
(RBAC, NetworkPolicies, image contract) renders against decided facts rather than assumed
ones — the §12-open-point-1 topology question especially. It implements issue #3513 and the
mode-3 half of #3482.

Companions own what this doesn't: [goobernetes-architecture.md](goobernetes-architecture.md)
(the substrate model + the §12 open points this closes), the constraint solver (#3506,
merged — eligible-runner-set output the dispatcher consumes),
[distributed-state-and-coordination.md](distributed-state-and-coordination.md) (the write
API the dispatcher's pods call), [goobernetes-restrictions.md](goobernetes-restrictions.md)
(#3516 — what the dispatcher stamps), and
[goobernetes-deployment-images.md](goobernetes-deployment-images.md) (the image contract).

---

## 1. Topology (closes architecture §12 open point 1)

**The dispatcher is a resident component in `goobers-system`, one instance per cluster,
running as a DISTINCT Deployment with its own ServiceAccount and minimal RBAC — NOT a mode of
the daemon process** (decision 011). Decided here, on these grounds:

- **It is control-plane, not workload.** It holds the credential-minting reach (it stamps
  stage-scoped credentials via the write API on the pod's behalf) and the Kubernetes
  `pods: create` authority. Per-gaggle dispatchers would replicate that authority into every
  gaggle namespace — the opposite of the structural-least-privilege the RBAC design (D-RBAC)
  builds on.
- **It shares the daemon's single-writer domain.** The dispatcher reads the resolved runner
  inventory and the pinned run definitions the daemon owns on the RWO instance root; a
  co-resident single instance in goobers-system avoids a second cross-namespace read path.
- **Per-gaggle fairness is a QUEUE property, not a process-location property** (D9: queues
  key (gaggle × runner-type)). One dispatcher serving all queues preserves fairness without
  N processes.
- **Distinct from the daemon, not a mode of it (decision 011):** a daemon-mode dispatcher
  would give the daemon's ServiceAccount `pods: create` in gaggle namespaces (it has zero
  grants today) and make the instance-root-mounting daemon pod the pod-creator — a
  blast-radius merge, not a transport detail. The dispatcher's API-server and Temporal egress
  ride its own identity; the daemon's egress (proxy-only) is unchanged.

**Consequence for infra, and it confirms the constraint (b) framing:** the dispatcher IS the
first `goobers-system` component to call the Kubernetes API server, and goobers-system is
deny-first. Its egress set (§4) must be rendered into goobers-system NetworkPolicies — the
per-gaggle-namespace alternative is off the table, so render against goobers-system's eight
policies, not per-gaggle ones.

*(The premise the disavowed document asserted as settled is now settled — but by this
document, sourced to the grounds above, not carried forward from it. The other two premises
it asserted, dispose-after-surrender and (gaggle × runner-type) queues, were already settled:
architecture D3 and D9 respectively.)*

## 2. What the dispatcher does per stage attempt

1. Receives the stage activity on its (gaggle × runner-type) queue.
2. Resolves the eligible runner from the solver's output (#3506 — an eligible-runner *set*;
   the dispatcher picks within it, Linux-preferring per the placement policy, and on a
   satisfiable-but-empty capacity waits within the bounded schedule-to-start, higher on
   Windows per D12).
3. Creates ONE fresh pod for the resolved runner from its host kind (`self` → local path,
   no pod; `image` → dispatcher-rendered spec; `deployment` → instantiated from the named
   template — DI-9), stamping: resource requests from `runsOn` quantities + limits from the
   runner ceiling (§5 for the tmpfs interaction), the mount-based restriction bindings
   (decision 006/007), exactly one **derived, non-overridable** `goobers.dev/runner-class`
   label (§3), the deny-first pod posture, and the OS node selector + Windows toleration.
4. Supervises the stage; relays liveness to the live journal.
5. Confirms output surrender (blobstore write-through + journal emits + ResultEnvelope) and
   **disposes the pod** — one attempt per pod, per D1.

## 2a. The artifact plane — network by digest, not a shared mount (decision 010)

PVCs are namespaced: a stage pod in a gaggle namespace **cannot mount `goobers-blobs`, which
lives in goobers-system**. So the mode-3 artifact plane is a NETWORK path — a stage pod
materializes (D6) and surrenders artifacts by fetching/putting **sha256 digests over the
network** from a blob endpoint, authenticated with a **stage-scoped credential minted via the
credential plane** (the podauth path the pod already uses). Not a shared PVC, not a
dispatcher-brokered byte-path. Blobs are already content-addressed and idempotent
(distributed-state §3, "identity not location"), which is the one property this needs and it
is already there. The gaggle namespace holds **no PVC** — `kubectl get pvc -n <ns>` stays
empty, so the instance-root-isolation structural invariant holds exactly as stated (§7), no
restatement. Backend is an implementation choice with the digest contract fixed: daemon-fronted
(reuses the control plane; makes the daemon a byte-path) or object-store-direct (Azure
Blob/Files, daemon-minted per-run-scoped credential — no byte-path, preferred at scale). v1
may start daemon-fronted. **This adds one pod egress destination (§4).**

**Blob-plane scoping is security-relevant (decision 012):** the blob grant **crosses
namespaces** — the stage pod lives in a gaggle namespace, the blob endpoint (v1: goobers-api)
in goobers-system — so each end's NetworkPolicy peer combines `namespaceSelector` **and**
`podSelector` in a **single** `to`/`from` element: the namespaceSelector reaches the other
namespace, the podSelector pins the one pod, the port pins the one port. It is **not** a
`podSelector`-only rule — that form is case A below and grants nothing. The three peer forms,
**measured on throwaway namespaces rather than argued** (`findings/evidence/netpol-peer-semantics.md`):

- **A — `podSelector` alone:** selects pods in the policy's OWN namespace, so across namespaces
  it matches nothing and grants **nothing**. It fails closed (safe), but the symptom is a stage
  that **hangs at materialize** — the decision-010 stall, presenting as a data-plane hang, not a
  visible policy denial. This is why "podSelector + port, never namespaceSelector" is wrong for a
  cross-namespace grant: followed literally it silently grants nothing.
- **B — `namespaceSelector` + `podSelector` in ONE peer (AND):** reaches exactly the blob
  endpoint's pod and port, nothing else. **Correct.**
- **C — the same two selectors as SEPARATE peers (OR):** every pod in the selected namespace,
  *plus* the selected pod in every namespace — the whole-namespace grant. In goobers-system that
  namespace holds the egress-proxy (a 0.0.0.0/0-except-RFC1918 allow), so C hands every stage pod
  a path to the proxy's allowlist (hosts no runner class's CIDR grant includes): the
  per-class-model **bypass** this decision forbids.

B and C differ by two characters of YAML indentation, and the difference is invisible on visual
review — so the composed peer is verified by **parsing it, not reading it** (a substring/grep
check is a correlate; the parsed peer is the observation). Both ends of the grant take the
single-peer AND form; if the backend moves off the daemon, its replacement gets its own
single-peer AND grant, never a separate-peer or namespace-wide rule. And **every runner class
carries the blob row, `restricted` included** — without it a restricted stage hangs at
materialize (not a policy denial, and indistinguishable at the pod from case A); it is the
class's own data path, not a grant to withhold.

## 3. The runner-class label — derived and non-overridable (decision 004, corrected)

The dispatcher stamps exactly one `goobers.dev/runner-class` label, **derived from the
resolved restriction set and non-overridable by workflow, gaggle, or stage input** — asserted
at dispatch, refuse-to-create on any attempt to influence it. RBAC cannot constrain label
*values*, so an input-influenced class label is privilege escalation into a broader egress
grant (the per-class NetworkPolicy model attributes egress by this label). This is the
load-bearing invariant, not the earlier "exactly one label" (a map key holds one value by
construction — a non-invariant).

**The derived VALUE is a single shared function (delivery decision 015):** the value string is
produced ONLY by `internal/runnercap.RunnerClassValue` — deterministic over the sorted resolved
restriction set, always a valid Kubernetes label value — used by BOTH the dispatcher's stamp and
the per-runner-class reference-manifest render, with a round-trip test asserting the two agree.
The three coupled label constants (`goobers.dev/runner-class`, `goobers.dev/role=stage`, and the
namespace marker `goobers.dev/gaggle-namespace=true`) are shared exported constants in the same
package, never literals; hand-authored downstream copies of the rendered manifests are
non-authoritative placeholders (delivery decision 016).

## 4. Egress set (constraint (b) — for goobers-system NetworkPolicy rendering)

The dispatcher's outbound needs, deny-first, to be rendered as goobers-system egress
allows:

| Destination | Purpose | Transport |
| --- | --- | --- |
| Kubernetes **API server** | pods create/delete/get/list/watch; pods/log get; resourcequotas/limitranges get/list/watch; apps/deployments **get** only (DI-9 template read) | HTTPS — but on AKS the API server is a MANAGED control plane (not a pod, a public Azure endpoint IP), so the only portable NetworkPolicy expression is an **ipBlock of the control-plane IP**, which Azure does not pin. It fails CLOSED to a total stage-plane outage when the IP moves, and provenance only catches that on re-render — so a **periodic live-endpoint drift preflight is required (#3560)**, not author-time provenance |
| **Temporal** `:7233` | poll the (gaggle × runner-type) activity queues | gRPC |
| Blobstore (dispatcher's own) | reads the resolved inventory / staging | **volume mount, NOT network** — no egress rule |
| The daemon write API | mint stage-scoped credentials for pods, journal emits | in-cluster to the daemon service (loopback if co-located; else goobers-system service) |

**Separately, the STAGE POD (not the dispatcher) egresses to the blob endpoint** (decision 010,
§2a) — by digest, stage-scoped credential; that egress is rendered on the stage-pod
runner-class policies (§7), not the dispatcher's. Nothing else on the dispatcher. In
particular the dispatcher does **not** read the container registry (the
image check is a tag comparison, decision 009 — no manifest read, no pull credential; the
kubelet pulls via the AcrPull identity, dispatcher only names the image).

## 5. Stamped pod spec — the interactions infra flagged

- **tmpfs sizeLimit set-and-budgeted (constraint (d)):** the decision-006 `tmp:ephemeral`
  mount is memory-backed `emptyDir`, which counts against the container memory limit
  (GA 1.22). The dispatcher sets the tmpfs `sizeLimit` explicitly (never default-to-half-node-
  RAM) and adds it to the container memory limit when stamping from the runner ceiling — so a
  stage filling `/tmp` fails with a budgeted, named limit, not a mysterious OOM against a
  ceiling that never accounted for it.
- **Restriction bindings by OS (decisions 006/007):** Linux `fs:readonly-except-workspace` =
  `readOnlyRootFilesystem: true` + writable workspace + writable HOME; Windows the same effect
  binds to `windowsOptions.runAsUserName: ContainerUser` and the dispatcher **must NOT stamp
  `readOnlyRootFilesystem` on Windows pods** (silently ignored, fails open — decision 007).
  `tmp:ephemeral` = ephemeral volume at the platform temp path (Linux /tmp; Windows the
  profile-nested temp path). `network:*` = the per-runner-class NetworkPolicy (rendered
  reference manifests, not dispatcher-applied — the operator holds no networking RBAC).
- **Orphan cleanup — NOT a cross-namespace ownerReference (constraint (a)):** the dispatcher
  is in goobers-system, pods in gaggle namespaces, and k8s GC deletes a dependent whose
  namespaced owner is in another namespace (silent-delete-reads-as-eviction). v1 mechanism:
  **`activeDeadlineSeconds` as the always-on backstop** (every stage pod carries one, derived
  from the stage timeout + a margin, so a dispatcher crash between create and the stage's own
  completion cannot leak the pod past its deadline) **plus a label + reconcile sweep** (the
  dispatcher labels every pod it creates with the run/attempt identity and its own owner
  identity and, on restart, reconciles the pods carrying ITS owner label). No ownerReference.
  This is a per-attempt-leak-bounded design, not a zero-leak one — acceptable for v1 since
  activeDeadlineSeconds caps the leak window.

  **The sweep's direction was reversed by decision 003** (graft: "owner label on
  dispatcher-created pods; `SweepOrphans` wired on the WORKER only, with a RunStates over
  Temporal Describe; the daemon never sweeps"). This section previously read "any labeled pod
  whose run is terminal or unknown is deleted" — fail-closed toward *deletion*. That is the
  hazard the record cites when it rejects the in-process-dispatcher option: with two drivers
  in the tree the sweep would delete pods belonging to live engine-start attempts, and a pod
  deleted mid-stage destroys in-flight work (invisibly, for a mutating stage like open-pr or
  merge-pr). The rule is now the other way round: a pod is disposed only when the sweep
  POSITIVELY establishes that no workflow is executing its attempt (Completed, Failed, or no
  such execution); a Running attempt is ADOPTED, and an unreachable engine, an unaddressable
  pod or any other uncertainty leaves the pod to `activeDeadlineSeconds`. Leaving a settled
  pod costs one stage timeout of capacity; deleting a live one cannot be undone.

  **The attempt's driver is stamped, never composed.** Each stage pod carries
  `goobers.dev/owning-workflow-id`: the id of the Temporal workflow execution whose activity
  created it, stamped by that workflow at dispatch (`workflow.GetInfo(ctx)`) and described
  verbatim by the sweep. It is the only identity the sweep asks about. Composing one from the
  pod's run and stage instead — `<run>/<stage>/<attempt>` for a `DispatchOne` driver, `<run>`
  for the engine's own walk — is correct for a directly started run and *wrong for a scheduled
  one*: `ClaimScheduled` starts the run's child workflow as `<claimID>-run` and `RunScheduled`
  then rewrites the run's RunID to `RunID(claimID)`, a sha256 prefix no describe can find (see
  `internal/engine/liveness.go`, which inverts the same mapping for claim liveness). Both
  composed ids then answer "no such workflow", which this sweep reads as *settled* — so it
  would delete the pod of a live scheduled stage, confidently and invisibly. On a delete path
  a lossy address is worse than none, which is also why a pod carrying no
  `goobers.dev/owning-workflow-id` is never disposed at all.

## 6. The version-skew check (decision 009 — tag comparison, publish-verified)

At dispatch the dispatcher compares its own embedded commit-sha to the image's **sha tag** —
a tag comparison that equals a commit comparison because the dev tag encodes the commit
(`goobers-base:<40-char-sha>`). No registry read. Soundness rests on a **publish-side stamp
gate covering the continuous-main sha-tag channel** — a named prerequisite (decision 009):
the release-engine publish path publishes no images today, and infra's `apply-tag.sh` gates
the release channel only. Until that gate exists for the sha-tag stream, tag-trust is only as
sound as the manual build discipline behind it — stated, not assumed.

## 7. What infra renders against this (the collateral hold released)

With topology decided (goobers-system, §1) the held (b) render is unblocked:
- **goobers-system NetworkPolicies** for the dispatcher's §4 egress set (the first
  goobers-system→API-server allow).
- **Dispatcher RBAC** (infra's `dispatcher-rbac.yaml`, already dry-run-clean) — namespaced
  Role per gaggle namespace, no PVC verbs (the instance-root-isolation structural invariant),
  the §4 verbs.
- **Per-runner-class egress policies** per decision 004/008 (CIDR-backed, per-class only, no
  `ipBlock.except`, the ratchet as the standing check) — network:allowlist v1 = "egress
  limited to these CIDRs" (decision 008).

## 8. Acceptance (the dispatcher's own; the e2e proof is #3517)

1. A mode-3 stage attempt runs in a fresh pod the dispatcher created, disposed after output
   surrender; no pod serves two attempts.
2. The runner-class label on every created pod is derived from the resolved restriction set;
   a workflow attempting to set it is refused at dispatch.
3. A dispatcher crash between create and stage completion leaks no pod past
   `activeDeadlineSeconds`; the restart reconcile sweep deletes any labeled orphan.
4. On a Windows pod, `readOnlyRootFilesystem` is NOT stamped; the fs restriction binds to
   ContainerUser (decision 007), proven by the denied-attempt test the restrictions epic owns.
5. The dispatcher's egress reaches only the §4 set, proven by the exit-code TRIPLE (not a
   bare denial, which a down host/partition also produces): a non-§4 host DENIED from the
   dispatcher (exit 28), the SAME host reachable from elsewhere (proving it is up), and a §4
   host reachable from the dispatcher (positive control). And the stage-pod blob egress
   (§2a) reaches the blob endpoint and nothing else, same triple.
6. A stage exceeding its budgeted tmpfs `sizeLimit` fails with a named limit error, not an
   unattributed OOM.
7. **Skew check (the mechanism §6 flags as resting on an unbuilt publish gate):** a stage pod
   whose image tag's sha does not equal the dispatcher's embedded commit is REFUSED at
   dispatch with a named diagnostic (the negative case, assertable today; the positive case
   is gated on the publish-side sha-tag stamp gate, decision 009).

## 9. Open implementation points

- Warm pools (deferred, D9/epic) layer on §2 later; the dispatcher's create path is the seam.
- The `deployment` host-kind template extraction contract (architecture §12 open point 2).
- The publish-side sha-tag stamp gate (decision 009 prerequisite) — owner TBD between the
  release engine (DI-7) and infra's apply-tag.sh adoption.
