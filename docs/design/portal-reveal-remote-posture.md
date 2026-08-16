# Scoping note: portal "reveal in Finder" and non-loopback (tier-2+) deployments

> Status: **draft — scoping note; not a build-ready design.** Placeholder issue #2306,
> follow-up to #2305 (the run-level reveal button itself, not yet implemented).
> This resolves the open questions #2306 lists so a future issue can be scoped
> and approved; it does not itself authorize implementation.

## 1. The problem, restated

#2305's proposed "reveal in Finder" button has the daemon shell out to open a
filesystem path on its own machine. That's only correct when the browser and
the daemon are on the same machine — true for today's default loopback bind
(`127.0.0.1:8080`). `internal/instance/config.go`'s `validateAPIConfig`
already permits a non-loopback bind for tier-2+ deployments (workstation/
shared-box/small-VM, `DEP-027`), gated behind TLS + an authenticator
(`SEC-043`). In that configuration, clicking reveal would open a window on
the *server's* desktop, not the requesting user's — silently wrong, not just
degraded.

## 2. How close is non-loopback portal access to real usage today?

**Not close.** `validateAPIConfig` requires `api.auth.oidc` to be configured
before it will accept a non-loopback bind — but per
[`docs/design/v1/38-auth-oidc-seam.md`](v1/38-auth-oidc-seam.md)'s own
progress note (2026-07-23), only the secret-resolver piece (A2) has shipped;
**the actual generic-OIDC authenticator (A1) — the thing that config option
points at — remains unbuilt.** `internal/httpapi`'s `Authorizer` seam exists
today only as tier-1's `AllowAll`. So a non-loopback bind is reachable in
config today but not meaningfully usable: there's no real authenticator
behind the gate yet. This is a **prerequisite-blocked** feature, not a
slow-adoption one — nobody can be running a real tier-2+ remote portal
deployment right now regardless of how compelling reveal-in-Finder is,
because the auth story it depends on isn't built.

## 3. Recommendation

**Detect non-loopback binding and disable the reveal action — do not build a
remote-aware alternative in this pass.**

Rationale:
- The remote-aware alternatives (a downloadable run-dir archive, a
  filesystem-independent "copy path") are each a real feature with their own
  design surface (archive format/size limits, streaming a directory over
  HTTP, path semantics when the viewer and daemon have different home
  layouts) — non-trivial work in service of a deployment posture that, per
  §2, has no real authenticated users yet.
- A detect-and-disable guard is cheap and already has the primitive it
  needs: `internal/instance/config.go`'s existing loopback check (the same
  one `validateAPIConfig`/`validateLoopbackListenAddress` already run) can be
  evaluated once at daemon startup and surfaced to the portal frontend as a
  boolean on whatever status/capability payload it already polls — no new
  detection logic, just one more field on an existing response and one
  conditional render in the run-detail page.
- This isn't a permanent dead end: when A1 (generic OIDC) ships and tier-2+
  remote access becomes something a real user actually does, that's the
  right time to scope the remote-aware alternative — informed by how people
  are actually using tier-2+ then, rather than speculatively today. Filing
  that as a fresh issue at that point is cheaper than building and
  maintaining an unused remote-access path now.
- Silently no-op-ing (the button appears but does nothing, or errors at
  click time) is worse than not offering it: a disabled/hidden control is
  the honest signal that the feature doesn't apply to this deployment. The
  portal already access-controls other interactive actions by deployment
  posture (`docs/requirements/portal.md` PORT-013 — approve/intervene
  gated by the auth ladder per tier), so gating this one on loopback-ness is
  the same shape of decision, not a new pattern.

## 4. What this unblocks

- #2305 (the reveal button itself) can ship with a one-line addition: gate
  its render on the same capability signal, rather than needing to design
  around the remote case at all. It should NOT block on this note beyond
  that one gating check — the remote-aware alternative is explicitly
  deferred, not a prerequisite.
- A future issue ("portal: remote-aware alternative to reveal-in-Finder for
  tier-2+ deployments") can be filed once A1 (generic OIDC) is closer to
  shipping, scoped against real usage rather than speculation. Not filed
  here since it's speculative until then — filing it now would just be
  another placeholder next to this one.

## 5. Non-goals of this note

- Does not design the capability-flag wire shape, the exact status endpoint
  to extend, or the frontend conditional — that's #2305's implementation
  detail once it picks up the one-line gate.
- Does not re-litigate the tier-2+ auth ladder itself (`38-auth-oidc-seam.md`
  owns that).
- Does not propose or design the remote-aware alternative — deferred per §3.
