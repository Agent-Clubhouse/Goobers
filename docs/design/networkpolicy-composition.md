# NetworkPolicy composition — the facts the egress model rests on

> Status: approved
> Delivered-by: #3568, #4294
> Tracking: #4294
> Scope-delta: None. This note documents the already-shipped netpol-render egress model; all five facts it set out to record are written down and no designed scope is deferred.
> Verified: eedb41ea6 (2026-10-02)

This note records the five facts about Kubernetes NetworkPolicy that the shipped
per-runner-class egress model rests on. It documents behavior the product already
relies on; it decides no new contract. An adopter who does not know these facts
tends to add a policy expecting it to subtract, to treat a CIDR allowlist as
enforcement, or to write a negative control that proves nothing.

Related: [Kubernetes infrastructure shape](k8s-infra-shape.md) (the deny-first
posture), [Goobernetes restrictions](goobernetes-restrictions.md) (the effect
model the policies render from), and the reference manifests in
`deploy/reference/`. Parents: #3301, #3568; the missing-half-policy shipped-manifest
fix is #3585.

## What ships

`goobers netpol-render` (package `internal/netpolrender`, entry `Render`) emits one
egress NetworkPolicy per distinct runner class from the instance `runners:`
inventory and the `egress.allowlist` CIDR groups (`api/schemas/instance.schema.json`).
Each policy selects stage pods on both `goobers.dev/role=stage` and
`goobers.dev/runner-class=<value>`. The `egress` check in `internal/k8spreflight`
dials the configured outbound targets from the checking host, and the
`networkpolicy-api` check reports whether the NetworkPolicy API is served. The gaggle-namespace
baseline is exactly a default-deny-all plus an allow-dns policy.

## 1. Policies are additive: no deny, no precedence

NetworkPolicy has no deny rule and no ordering. The effective egress of a pod is
the union of every policy that selects it. A broad allow therefore makes every
narrower policy a no-op, silently: nothing errors, the narrow policy is simply
never the limiting one.

The practical consequence is a rule for authors: never grant egress to
`goobers.dev/role=stage` without also pinning a runner-class label. A generic
stage-wide grant would union over, and nullify, every per-class restriction. The
shipped manifests keep to this: the renderer produces the stage egress grants,
and each rendered grant selects exactly one class. `TestDeployReferenceRenderedTogether` (in
`cmd/goobers`) renders the reference bases together and fails any egress policy
that selects `role=stage` without a runner-class label; that assertion encodes
the consequence, and this section is the reason for it.

To restrict a class further, remove the grant from the policy that supplies it.
Adding another policy can only widen access.

## 2. CIDR allowlists fail open for shared (CDN) destinations

Routing and enforcement are two layers. Destinations behind shared CDN address
space cannot be separated by address: published CIDR sets for different services
of one provider overlap, so an address that a restricted class must not reach can
be byte-identical to one it must. A CIDR allowlist can therefore only mean
"limited to these CIDRs", never "limited to this service".

Per-FQDN policy needs a forward proxy in front of the stage pods, and the proxy
must not intercept TLS. A CIDR group that names an upstream source carries a
provenance marker so rotation of the published set is detected rather than
discovered as a mid-run timeout (see `CheckProvenance` in
`internal/netpolrender`). The renderer also refuses unfilled documentation-CIDR
placeholders instead of shipping stubs.

## 3. A negative control proves a denial, not a denier

If a restricted-runner test is satisfied by a namespace-wide default-deny, the
test passes without the restricted-runner policy existing. Write the control so
that it can only pass because of the specific policy under test:

- Assert on the specific policy's effect: the same probe succeeds when that
  policy permits it and fails when it does not, from a pod that differs only by
  the class label.
- Assert on evidence, never on exit status. A tool can exit 0 while every
  request it made failed (a telemetry generator is the canonical case); check
  that the request was received or refused, not that the process returned.

This is the shape the `networkpolicy-api` preflight check says is missing. That
check is read-only and only discovers that `networking.k8s.io/v1` serves
`networkpolicies`; a CNI can serve the API and still ignore policies, so the check
reports a warning and never a pass until a denied attempt proves enforcement.

## 4. The peer forms look alike and mean different things

In an ingress `from` or egress `to` list:

| Form | Meaning |
|---|---|
| `namespaceSelector` and `podSelector` in the same list element | AND: the named pod(s) in the matching namespace(s) |
| `namespaceSelector` and `podSelector` as two list elements | OR: every pod in the matching namespaces, plus the matching pods in every namespace |
| `ipBlock` | A CIDR: matches by address, not identity. Pod IPs change and may be rewritten, so it cannot name a pod or namespace |
| `podSelector` alone | Pods in the policy's own namespace only; for a cross-namespace destination it matches nothing and grants nothing |

The AND and OR forms differ by two characters of YAML indentation. The first is
the correct cross-namespace grant; the second is an egress-proxy bypass. The
renderer builds every cross-namespace peer through `composeCrossNamespacePeer`,
and its tests parse the composed peer rather than grep for it for the same reason.

## 5. Both ends of a cross-namespace flow need a policy

When both namespaces default-deny, a flow needs an egress allow on the source side
and an ingress allow on the destination side. The missing half presents as a
connect timeout, indistinguishable from a correct denial, so the failure is easy
to misdiagnose as a routing problem. #3585 fixed the shipped-manifest half of
this: when adding a destination to a class's egress, check that the destination
namespace admits the flow.

## Where this is referenced

- `deploy/reference/README.md`, Conventions.
- The `networkpolicy-api` check hint in `internal/k8spreflight`.
