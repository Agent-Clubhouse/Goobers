# Tier-2 OIDC authentication

Tier 1 relies on local machine trust: the daemon listens on
`127.0.0.1:8080` by default and does not require credentials. Keep that
posture when every caller is local to the host.

Enable OIDC before the API is reachable by another machine or trust domain.
A reverse proxy, tunnel, port forward, container-published port, or non-loopback
listener all count as exposure. OIDC authenticates each request; TLS protects
the bearer token in transit.

## Choose the listener posture

For a local-only instance, omit `api` or keep an explicit loopback listener:

```yaml
api:
  listen: 127.0.0.1:8080
```

This listener is plain HTTP and unauthenticated. Do not publish or proxy it.

For a directly exposed listener, Goobers requires both TLS and OIDC and has no
insecure override:

```yaml
api:
  listen: 10.0.0.7:8443
  tls:
    certFile: /etc/goobers/tls/api.crt
    keyFile: /etc/goobers/tls/api.key
  auth:
    oidc:
      issuer: https://id.example.com/realms/platform
      audience: goobers-api
      rolesClaim: groups
      roles:
        view: [goobers-viewers]
        operate: [goobers-operators]
        admin: [goobers-admins]
```

Bind a specific private address where possible and place the instance behind
the normal firewall or ingress controls. The certificate must be valid for the
name clients use.

An edge proxy may instead terminate TLS and forward to a loopback listener.
Keep `api.auth.oidc` configured in that arrangement so the daemon still
validates every request; omitting `api.tls` is valid only because the daemon
socket itself remains on loopback. Restrict the proxy route to trusted hosts
and never let clients bypass the TLS edge.

## HTTP Basic Auth at the edge is not a substitute for `api.auth.oidc`

Some operators add an edge-proxy HTTP Basic Auth gate (an ingress
`auth-basic` annotation, an nginx `auth_basic` block, and similar) in front
of the single inbound door that serves the portal and `/api/*` together (see
[the Kubernetes infrastructure shape](../design/k8s-infra-shape.md#5-networking)).
This works mechanically, but it is worth being precise about *why*, because
an earlier note (from the `spike/goobernetes` exploratory deployment) claimed
Basic Auth "breaks" the portal because its own requests "never carry the
credential." That claim does not hold up under reproduction and is corrected
here.

**Reproduced:** running the built `goobers dashboard` behind a minimal
Go `httputil.ReverseProxy` that requires HTTP Basic Auth on every path shows
that with no `Authorization` header, `/`, `/api/v1/health`, and the
long-lived `/api/v1/events` stream all return `401` with a
`WWW-Authenticate: Basic` challenge, uniformly. With the credential supplied,
all three succeed — including the SSE stream, which keeps delivering journal
events through the proxy for the life of the connection.

**Why:** the portal's data client (`portal/src/api/httpClient.ts`) issues
only relative, same-origin `fetch()` requests for both ordinary reads and
the event stream — it does not use the native `EventSource` API (which
cannot attach custom headers) and never sets an explicit `credentials`
option. `fetch()`'s default credentials mode, `"same-origin"`, includes the
browser's cached HTTP Basic Auth credential automatically once the browser
has answered the native prompt for the page itself, independent of that
default. That native prompt appears only for the top-level page load; a
background `fetch()` that later receives a `401` does not raise another
prompt, it simply resolves as a `401` to the app. Nothing in the request
path strips the header either: neither Go's `httputil.ReverseProxy` (used by
both `goobers dashboard`'s own daemon-attach proxy and this style of edge
proxy) nor nginx's `auth_basic` module removes `Authorization` before
forwarding upstream.

**What can actually go wrong, and what Basic Auth does not give you:**

- Protecting only part of the origin (for example an ingress rule that
  applies `auth-basic` to `/` but not `/api/`, or that uses a different
  realm per path) produces inconsistent `401`s, because the browser's
  credential cache is scoped per origin+realm. Apply the gate to the whole
  host, matching the single-inbound-door shape above, not to a subset of
  paths.
- Basic Auth at the edge is a network-level gate only. It does not give the
  daemon any notion of *who* is calling — without `api.auth.oidc`
  configured, the daemon still authorizes every request (`NullAuthenticator`
  / tier-1 `AllowAll`). Use it, if at all, to keep unauthenticated traffic
  off a URL that is otherwise reachable; use `api.auth.oidc` (this guide)
  when the daemon itself needs per-user identity and role checks.
- How the portal's UI reports a `401`/`403` it does receive (rather than
  whether the credential reaches the daemon) is tracked separately.

## Register the OIDC applications

Configure the identity provider with:

1. An issuer whose discovery document is available at
   `<issuer>/.well-known/openid-configuration`.
2. An API resource or audience for Goobers. Issued access tokens must carry
   this exact value in `aud`.
3. A browser public-client registration for the portal. Record its client ID
   and register the exact external portal URL as a redirect URI, for example
   `https://goobers.example.com`.
4. A role or group claim with values that can be mapped to Goobers roles.

The issuer value must exactly match the token's `iss` claim. Production issuers
must use HTTPS; HTTP is accepted only for a loopback development issuer. The
browser client is public, so do not create or place a client secret in
`instance.yaml` or a portal build.

Some providers use the browser client ID as the access-token audience; others
use a separate API identifier. Set `api.auth.oidc.audience` to the value
actually emitted in the access token, not automatically to the portal client
ID.

## Configure the daemon

The shipped `instance.yaml` settings are:

| Setting | Purpose |
| --- | --- |
| `api.listen` | Daemon `host:port`; a non-loopback host requires TLS and OIDC |
| `api.tls.certFile` | TLS certificate path for a directly exposed listener |
| `api.tls.keyFile` | TLS private-key path for a directly exposed listener |
| `api.auth.oidc.issuer` | Exact token issuer and OIDC discovery base URL |
| `api.auth.oidc.audience` | Required token `aud` value |
| `api.auth.oidc.rolesClaim` | Claim containing role values; defaults to `roles` |
| `api.auth.oidc.roles.view` | Claim values allowed to read the API |
| `api.auth.oidc.roles.operate` | Claim values allowed to read and mutate |
| `api.auth.oidc.roles.admin` | Claim values granted the strongest role |

At least one role value is required. Map each claim value once. Roles are
ordered: `admin` includes `operate` and `view`, and `operate` includes `view`.
An authenticated principal with no mapped value is denied.

Validate the instance before restarting the daemon:

```sh
goobers validate /srv/goobers
goobers up /srv/goobers
```

The daemon discovers signing keys lazily. If discovery or key retrieval fails,
requests fail closed rather than falling back to anonymous access.

## Configure the portal client

The portal authentication seam reads these Vite variables at build time:

| Variable | Purpose |
| --- | --- |
| `VITE_OIDC_ISSUER` | Same issuer/authority configured for the daemon |
| `VITE_OIDC_CLIENT_ID` | Browser public-client ID |
| `VITE_OIDC_REDIRECT_URI` | Registered redirect URI; defaults to the browser origin |

Both issuer and client ID must be set together. If a redirect URI is set, it
must exactly match a URI registered with the provider. Because these values are
compiled into the static bundle, changing them requires rebuilding the portal:

```sh
VITE_OIDC_ISSUER=https://id.example.com/realms/platform \
VITE_OIDC_CLIENT_ID=goobers-portal \
VITE_OIDC_REDIRECT_URI=https://goobers.example.com \
npm --prefix portal run build
```

The `goobers dashboard` attach proxy still cannot supply a bearer token to an
authenticated running daemon, and the portal data client does not yet attach
the token produced by the authentication seam. Do not treat these build
variables as a working access control for that attached-to-a-live-daemon
case — it stays loopback-only; stop the daemon or query its API directly if
you need authenticated access to it through the portal.

The standalone dashboard (no live `goobers up` daemon reachable) is
different: `goobers dashboard --listen <host:port>` accepts a non-loopback
host once `api.auth.oidc` is configured, gated the same way `api.listen` is
(SEC-043) — there is no insecure override, and the standalone handler
validates the same bearer tokens the daemon API does. Loopback stays the
default; `--listen` to a non-loopback host without `api.auth` configured is
refused outright.

## Verify authentication and authorization

Use a short-lived access token from the configured issuer. Never put it in
shell history or a configuration file. Curl's `--oauth2-bearer` option sends
the token with the required RFC 6750 `Bearer` authorization scheme.

```sh
curl --cacert /etc/goobers/tls/ca.crt \
  -o /dev/null -w '%{http_code}\n' \
  https://goobers.example.com:8443/api/v1/health

curl --cacert /etc/goobers/tls/ca.crt \
  --oauth2-bearer "$GOOBERS_ACCESS_TOKEN" \
  https://goobers.example.com:8443/api/v1/health
```

The request without a token returns `401`. A valid token with a mapped
`view`, `operate`, or `admin` value can read the health endpoint. A valid token
without a mapped value returns `403`; a wrong issuer, audience, signature, or
expired token returns `401`.

## Upgrade to the tier-3 Azure posture

The authentication seam does not change at tier 3. Configure Microsoft Entra
ID as the issuer, using its tenant-specific OIDC issuer, API audience, public
client ID, redirect URI, and app-role or group claim values in the same fields
above. Tier-3 RBAC strengthens authorization without introducing an
Entra-specific authenticator; per-gaggle workload identities remain separate
from human OIDC login.

Move operational credentials from env/file refs to Azure Key Vault through the
same secret-resolver seam. The
[external secret stores guide](secret-stores.md) covers `secretStores`,
`store:` token refs, workload or managed identity, caching, and rotation. OIDC
issuer, audience, client ID, and redirect URI are identifiers rather than
secrets; bearer tokens and TLS private keys still require normal secret
handling.

See the [security and auth ladder](../ARCHITECTURE.md#9-security-and-auth-ladder)
and the [OIDC seam design](../design/v1/38-auth-oidc-seam.md) for the
cross-tier architecture.
