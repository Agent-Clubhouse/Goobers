# Reusable Goobers portal package

Build the existing portal as an **npm-format release tarball**, without copying
its source into a consuming repository:

```powershell
Set-Location portal
npm ci
npm run package:portal
npm run test:package
```

Output is `.portal-package/goobers-portal-<version>.tgz` and a SHA-256 sidecar.
The package is marked private to prevent accidental registry publication;
installing the tarball works normally. React and ReactDOM are peer dependencies,
so the host owns a single React runtime. Three.js remains a normal dependency.
The standard daemon `npm run build` output and Go embedding path are unchanged.

## Host contract

```tsx
import {
  PortalWorkbench, HttpDaemonClient, publishReadState,
} from "@goobers/portal";
import "@goobers/portal/styles.css";

const client = new HttpDaemonClient({
  baseUrl: "/selected-instance",
  onReadState: publishReadState,
});

<PortalWorkbench
  client={client}
  scope={`${principalId}:${instanceId}:events:all`}
/>;
```

The host must implement the complete `DaemonClient` interface or provide a
compatible same-origin HTTP/SSE surface to `HttpDaemonClient`. Fleet can supply
its own client adapter, including authentication, route translation and errors.
There is no implicit localhost client, identity provider or authorization policy
in `PortalWorkbench`. The scope is non-secret and must change across principal,
instance, route or filter changes; it isolates saved SSE cursors and remounts
the workbench. The embedded workbench reconnects SSE with backoff and **does not
fall back to periodic polling**. The local daemon portal retains its existing
polling policy and legacy cursor key.

This first package supports **one full-document workbench at a time**.
It owns the hash router, global CSS/theme and document title/favicon. It is not
a multi-instance microfrontend or SSR component. Copy the package's `assets/`
files into the host's static root; existing image URLs remain root-relative.
Host-supplied branding remains supported. A host needing arbitrary subpath asset
mounts or simultaneous workbenches must extend these contracts first.

The package does not make a limited relay into the full portal API. In
particular, a relay supporting only health/instance/runs/events must not claim
to support every page; broader allowlisted route coverage is separate work.

## Distribution choice

Use the same npm tarball for both release and local development. A separate
package feed is not necessary for the first consumer:

| Option | Advantages | Cost |
| --- | --- | --- |
| GitHub Release asset | Repository permissions, no new feed, immutable source/version pin | Explicit artifact download before installation |
| npm/GitHub Packages feed | Familiar dependency resolution and update tooling | Registry setup, cross-repository read permissions, token management |
| Source copying or Git dependency | Initially simple | Blurs build/version/provenance boundaries; not the chosen delivery |

The portal-package workflow uploads the tarball, manifest and hash as a CI
artifact for PR/development builds. A maintainer can dispatch a versioned release
from clean main; stable releases use the portal version, prereleases can use an
explicit SemVer version. Existing release assets are not overwritten.
No npm registry publication or new Azure infrastructure is involved.

For a consuming repository, download an **exact** release tag/asset, verify the
SHA-256 against the reviewed pin, and install its tarball with `npm install
--save-exact <local-tarball>`. Commit the dependency and lockfile integrity,
not the binary. In CI, download to the same ignored relative path before
`npm ci`. If the source repository is private, an internal consumer needs a
read-only GitHub App/installation token with access to that repository; its
default per-repository workflow token is not automatically cross-repository.
Keep download credentials out of `.npmrc`, artifacts and URLs in the lockfile.

Development builds default to `<portal-version>-dev.<source-sha>` and record
`dirty: true` when appropriate. Use a locally built tarball override without
committing that machine-specific dependency/lockfile change. This avoids
`npm link` accidentally introducing a second React instance. Use the same
package-building command in both repositories' development loops; no registry
is required. Never promote a dirty development artifact to a release pin.

`portal-artifact.json` records source SHA, package version, dirty state,
generated API-contract hash, lockfile hash and host contract. It is provenance
metadata, not a cryptographic attestation: verify the downloaded asset against
the independently reviewed release hash.
