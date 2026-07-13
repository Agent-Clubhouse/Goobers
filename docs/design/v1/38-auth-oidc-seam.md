# Design: Auth seam — optional OIDC for team instances — V1 epic #38

> Status: **Draft for review** · Area prefix: `SEC` · Milestone: **V1**
> Requirements: [`docs/requirements/security.md`](../../requirements/security.md)
> (SEC-043, auth ladder) · Architecture: [`docs/ARCHITECTURE.md`](../../ARCHITECTURE.md) §9
>
> Detailed-design artifact for epic **#38**. The dispatchable work items (A0–A3)
> each link back to the correspondingly-named section here.

## 1. Verdict

**This is a seam epic — mostly about shape, not volume.** The doctrine (security.md §
intro): *"The protocol (OIDC) and the seams (an `Authenticator` + a secret-resolver
interface) are constant; tiers select implementations."* Two facts from the V0 code:

- **The secret-resolver seam already anticipates V2.** `credentials.Resolver.Resolve(ctx,
  name)` ([`internal/credentials/source.go`](../../../internal/credentials/source.go))
  explicitly documents that `ctx` exists "for future resolvers... e.g. a Key Vault client
  at V2." Finalizing it is **interface-hardening, not building Key Vault** (which is V2).
- **The portal already has Entra-specific auth scaffolding** (`portal/src/auth/msal.ts`,
  `AuthGate.tsx` on `@azure/msal-react`) — but Entra is the **tier-3 (V2)** rung. There is
  **no `Authenticator` seam on the Go daemon** at all. #38 introduces the generic seam and
  a **generic OIDC** implementation for tier 2, with Entra reframed as one *configured
  issuer* of the same seam at V2.

**Off at tier 1, generic OIDC at tier 2, Entra at tier 3 (V2) — all the same seam.**

## 2. Scope boundary

**In scope (V1, tiers 1–2):** a single `Authenticator` seam on daemon + portal; a generic
OIDC implementation (configurable issuer) that is **off by default** (tier 1 = no auth);
finalization of the secret-resolver interface so a V2 Key Vault resolver drops in without
caller changes; tier-2 auth setup docs.

**Out of scope — V2 (do NOT build):** Entra ID / MSAL as *the* auth (it becomes a
configured issuer + RBAC at tier 3); Azure Key Vault resolver; per-gaggle cloud identities.

## 3. Architecture

```
Authenticator seam (constant across tiers):
  tier 1 → NullAuthenticator (no auth, default)
  tier 2 → GenericOIDC(issuer, clientID)         ← this epic
  tier 3 → Entra (OIDC issuer + RBAC)            ← V2 config of the SAME seam

daemon API (#37 P1): Authenticator validates the bearer token → #37 P4 Authorizer decides
portal (#37 P3):     generic OIDC login → sends token to the daemon API

secret-resolver seam (constant): Resolve(ctx, name)
  tier 1–2 → env/file (exists)      tier 3 → Key Vault (V2, drops in unchanged)
```

`Authenticator` (authN) pairs with #37's `Authorizer` (authZ, P4) — **the same seam
family**; they must be designed together. This epic owns the authN half + the OIDC issuer.

## 4. Missions (dispatchable, single-PR-sized)

### A0 — `Authenticator` seam (daemon + portal) + tier-1 no-auth default
- Define the Go `Authenticator` interface on the daemon API (pairs with #37 P4's
  `Authorizer`); `NullAuthenticator` as the tier-1 default. On the portal, generalize the
  Entra-specific MSAL wrapper into a pluggable auth seam (no issuer hardcoded).
- **Seams:** daemon API middleware (shared with **#37 P4**), `portal/src/auth/*`.
- **Test plan:** tier-1 default authenticates nobody-required (open); the seam accepts a
  fake authenticator that rejects/accepts; portal seam builds with no issuer configured.

### A1 — Generic OIDC implementation (tier 2)
- A generic OIDC `Authenticator` (recommend `github.com/coreos/go-oidc` for token
  validation) with a **configurable issuer** — off at tier 1, on at tier 2. Portal: generic
  OIDC login (generalize `AuthGate` to any issuer). Entra satisfies this as a configured
  issuer (its tier-3 RBAC is V2). Depends on **A0**.
- **Seams:** daemon `Authenticator` impl, `portal/src/auth/*`, config (issuer/clientID).
- **Test plan:** valid OIDC token → authenticated; expired/wrong-issuer/wrong-audience →
  rejected (fail-closed); disabled by default (tier 1 unaffected); portal login round-trips
  against a fake issuer.

### A2 — Secret-resolver seam finalization
- Promote `credentials.Resolver` to a stable **interface** (`Resolve(ctx, name)`) so a V2
  Key Vault resolver drops in without caller changes; document the drop-in point (SEC-010).
  No Key Vault implementation.
- **Seams:** `internal/credentials/source.go`, caller wiring.
- **Test plan:** existing env/file resolver satisfies the interface with no behavior change;
  a fake async resolver plugs in via the interface; `ctx` cancellation honored.

### A3 — Tier-2 auth setup docs + posture
- Operator guide: when to enable OIDC (exposed beyond loopback), issuer/clientID/redirect
  config, and the "loopback-only vs exposed" decision. Documents the auth ladder rung 2 and
  the V2 (Entra/Key Vault) upgrade path.
- **Seams:** docs.
- **Test plan:** doc-lint/link-check; the setup steps match A1's actual config keys.

## 5. End-to-end / integration test

Daemon API (#37 P1) with the generic OIDC `Authenticator` enabled: a request with a valid
fake-issuer token is authenticated and (via #37 P4) authorized; an invalid token is rejected
fail-closed; with auth disabled (tier 1) the same request is open. No network — fake issuer.

## 6. Dependencies & coordination

- **Pairs with #37 P4** (`Authorizer`) — same daemon-middleware seam; design A0 and P4
  together to avoid two incompatible auth hooks.
- **Coordinates with #37 P3** (portal retarget) — both touch `portal/src/auth/*`; sequence
  so the portal isn't rewritten twice.
- Consumed by **#34** (team multi-gaggle instances exposed beyond one machine).

## 7. Open questions (for PM / PO)

- **OQ-1 — keep MSAL or go generic-only:** replace the existing Entra-specific MSAL portal
  path with a generic OIDC client (Entra as a documented issuer), rather than maintaining two
  auth stacks? *(Recommend: generic-only in V1; Entra is a V2 issuer config.)*
- **OQ-2 — OIDC library:** `coreos/go-oidc` on the daemon + a standard OIDC client on the
  portal. *(Recommend: yes.)*
- **OQ-3 — token model:** portal does OIDC login and sends a bearer token the daemon
  validates per request (stateless). *(Recommend: yes; loopback-only stays no-auth.)*
- **OQ-4 — A0/#37-P4 ownership:** should A0 and #37 P4 be a **single** combined mission to
  keep authN+authZ coherent? *(Recommend: keep separate but co-review; flag to PM for dispatch
  ordering.)*
