# CI runtime acquisitions

An executable being present on `PATH` does not mean it can run without network
access. Go can acquire modules and a toolchain, npm can fetch packages or audit
data, and the Temporal SDK can download its own dev-server executable.

The checked-in [runtime acquisition manifest](../../test/ci/runtime-acquisitions.json)
records **what**, **from where**, and **the local override or absence of an
offline path**, together with the executable sites that require it. Read it
alongside the platform prerequisites in [CONTRIBUTING](../../CONTRIBUTING.md).
It includes hosted CI bootstrapping as well as local merge-tier requirements;
an entry for a hosted action does not mean a local developer needs that action.

## Before running in an egress-controlled environment

| Acquisition | Local preparation | Important boundary |
|---|---|---|
| Go modules and toolchains | Provision a local file proxy containing selected versions and metadata; install a compatible Go and set `GOTOOLCHAIN=local` | `GOPROXY=off` can still fail for cached `go run module@version` tools because their metadata lookup is disabled. Source archives alone are insufficient. |
| npm packages | Prewarm the exact lockfile closure; use `npm_config_offline=true` | An installed `node_modules` directory alone is insufficient for `npm ci`, which reconstructs it. |
| npm audit | Provide access to the configured advisory service | **No offline path.** The mandatory audit remains enabled; a warm package cache cannot replace current advisory data. |
| Playwright Chromium | Preinstall the exact revision and its completion marker under `PLAYWRIGHT_BROWSERS_PATH` | The local CI runner checks both before skipping installation; a read-only baked browser tree is supported. System libraries are a separate acquisition. |
| Temporal dev server | Set `GOOBERS_TEMPORAL_CLI` to an existing compatible executable | The helper selects SDK `ExistingPath`, clears `CachedDownload`, and does not fall back to a download if that path fails. |
| Kubernetes envtest | Set `ENVTEST_INSTALLED_ONLY=1` to select installed assets; add `ENVTEST_USE_ENV=1` and `KUBEBUILDER_ASSETS` for an explicit directory | A missing installed version fails without downloading. Provisioning the setup tool's own Go modules is separate. |
| golangci-lint | Install the pinned version on `PATH` before local CI | The hosted setup action downloads its installer and has no offline switch. |
| Go vulnerability database | Override the Make `GOVULNCHECK` command with the pinned tool plus `-db file:///absolute/database/path` | Use a complete, current, trusted database mirror, never an empty replacement. The pinned tool takes `-db`, not a `GOVULNDB` environment override. |
| Kubernetes schemas | Override the Make `KUBECONFORM` command with the pinned tool plus `-schema-location /absolute/registry/template` | Provide every needed schema locally; do not include `default` or HTTP fallback locations, and do not skip missing schemas. |

The manifest separately records apt, Chocolatey, Maven, opt-in NuGet restore,
browser system libraries, and external GitHub Actions. Some hosted steps have cache fast paths but retain
network-backed miss paths. They are marked `no-offline-path` where the shipped
invocation does not expose an offline mode. This is visible debt, not permission
to skip a required check. In particular, the complete merge gate is **not**
advertised as air-gap compatible while its npm audit requires a service.

## Enforcement and updates

`runtime-acquisitions` is a preflight check, before the long unit and browser
suites. To run its tests directly:

```sh
go test ./test/ci -run 'Test.*Acquisition' -count=1
```

Discovery is independent of the manifest. It examines the executable merge-check
list for all three platforms, Makefile recipes and tool definitions, Portal
package scripts, CI workflow
steps and referenced local composite actions, and Temporal SDK references in Go
source (including tests and aliased imports). Every executable in the declared
integration needs-list also requires an explicit acquisition decision, with a
reason if its shipped fixture performs no lazy dependency acquisition. A new
discovered site with no
declaration fails; a removed site left in the manifest also fails. Tests add new
commands and SDK references without adding declarations to verify that boundary.
Multiple acquisitions in one script have distinct occurrence identities. Direct
download sites also carry the SHA-256 of the normalized script, so changing a
URL or download argument in an existing step requires manifest review. On drift,
inspect the command and update its origin/override description as needed before
copying the new site identity from the diagnostic into the manifest.

When adding an acquisition, inspect its actual invocation and record its origin,
override, and discovered sites. Test that the override prevents the download;
do not infer that from an environment variable's name. If no working override
exists, say `no-offline-path` and explain which invocation still needs network.

This static check is not a network sandbox or a proof of arbitrary third-party
code's behavior. External Actions are declared as opaque bootstrap dependencies;
their transitive network behavior is not audited by this scanner. New acquisition
mechanisms also require a discovery rule and a negative regression test, not just
a prose addition to this document.
