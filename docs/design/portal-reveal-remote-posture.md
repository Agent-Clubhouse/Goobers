# Scoping note: portal "reveal in Finder" and non-loopback (tier-2+) deployments

> Status: **implemented — §3's recommendation was adopted.** Placeholder issue
> #2306 remains open for the *remote-aware alternative*, which is still
> deliberately deferred (§3, §4). Reconciled against the tree 2026-09-06 by
> [#4522](https://github.com/Agent-Clubhouse/Goobers/issues/4522).
> Delivered-by: #2305, #2884

> ### What shipped, and what is now stale here
>
> **The reveal button shipped** (#2305, CLOSED — route
> `apicontract.RunRevealPath`, `internal/httpapi/reveal_test.go`, capability
> `RevealRun` on `readservice.PortalConfig`, gated in `RunPage.tsx`), landing on
> 2026-08-03 — the same day this note was written.
>
> **§3's recommendation was adopted, in exactly the shape §3 asked for.** The
> revealer is wired only when the listener is loopback, at both entry points:
> `cmd/goobers/up.go` gates `httpapi.WithRunRevealer` on
> `instance.IsLoopbackListenAddress(apiListenAddress(...))`, and
> `cmd/goobers/dashboard.go` gates it on the standalone listener's own
> loopback-ness (#2884). The capability then falls out of the wiring —
> `Capabilities.RevealRun = config.runRevealer != nil` — and the portal
> conditions the control on it. That is "evaluate once at startup, surface as a
> boolean on the payload the portal already polls, one conditional render",
> which is what §3 specified.
>
> **Two premises in §1–§2 are stale and should not be relied on:**
>
> - There is **no `validateAPIConfig`**. The listener check is
>   `validateLoopbackListenAddress` plus the `api.listen` rule in
>   `internal/instance/config_validation.go`, and that rule now requires
>   **`api.tls`** for a non-loopback bind, *not* OIDC. Requiring OIDC
>   specifically was removed deliberately, because it made the most restrictive
>   posture — zero human access, pod plane only behind `DenyAllAuthenticator` —
>   the one posture that could not be expressed.
> - **§2's "prerequisite-blocked" argument no longer holds.** A1, the generic
>   OIDC authenticator, shipped: `internal/oidcauth` is a real implementation
>   wired into both `goobers up` and `goobers dashboard`. A non-loopback,
>   TLS-terminated, OIDC-authenticated portal is a configuration a real operator
>   can run today. The loopback gate above is therefore load-bearing, not
>   precautionary — which is the opposite of what §2's reasoning implied and the
>   reason it is worth correcting rather than deleting.
>
> **What is still open (#2306):** the remote-aware alternative — a downloadable
> run-directory archive, or a filesystem-independent "copy path". §3 deferred it
> until tier-2+ remote access had real users. With A1 shipped, the condition §3
> named as the trigger for re-scoping is now closer to met, so #2306 should be
> re-scoped against actual tier-2+ usage rather than left as a placeholder
> indefinitely.

## 1. The problem, restated

#2305's proposed "reveal in Finder" button has the daemon shell out to open a
filesystem path on its own machine. That's only correct when the browser and
the daemon are on the same machine — true for today's default loopback bind
(`127.0.0.1:8080`). Instance validation already permits a non-loopback bind for
tier-2+ deployments (workstation/shared-box/small-VM, `DEP-027`), gated behind
TLS (`SEC-043`). *(The note originally named `validateAPIConfig` and described
the gate as TLS + an authenticator; see the banner — the function does not
exist and the rule is TLS.)* In that configuration, clicking reveal would open a window on
the *server's* desktop, not the requesting user's — silently wrong, not just
degraded.

## 2. How close is non-loopback portal access to real usage today?

**Not close.** *(Stale as of 2026-09-06 — see the banner. A1 shipped and this
answer is now "close enough that the §3 gate is load-bearing".)* The listener
rule required `api.auth.oidc` before it would accept a non-loopback bind — but per
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
