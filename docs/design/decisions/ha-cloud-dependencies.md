# HA cloud dependency decision

- Status: approved
- Date: 2026-10-21
- Decision owner: issue #7711
- Programme: control-plane HA, phase P1

## Context

The HA programme introduces a separate `goobers-cloud` binary backed by
PostgreSQL and object storage. The approved design adopts two binaries (D-4)
and Azure Blob Storage first, deferring S3-compatible storage until a non-Azure
target is committed (D-7). This record confirms those defaults at P1 entry.
The design is being landed by
[#7725](https://github.com/Agent-Clubhouse/Goobers/pull/7725).

New dependencies must preserve the repository's existing boundaries:

- Dependencies are resolved and baked at build time. Production binaries never
  run `go get`, download plugins, or otherwise acquire code at runtime.
- [SEC-048](../../requirements/security.md#no-phone-home-guard-and-compliance-audit)
  forbids maintainer-facing telemetry and built-in collection endpoints.
  Storage and database destinations come only from operator configuration.
- `make tidy-check`, `make vulncheck`, licence review, and the
  [runtime-acquisition inventory](../../guides/runtime-acquisition.md) remain
  mandatory.
- Cloud-only modules may be imported only by `cmd/goobers-cloud` and cloud
  packages. They must not enter the package or selected-module closure of
  `./cmd/goobers`. `TestGoobersClosureExcludesCloudDeps` in #7714 enforces that
  boundary.
- Integration services run over TCP in CI service containers under the
  `integration` build tag. Unit tests use fakes and conformance suites; they do
  not start containers or require network access.

## Evidence method

The review snapshot is 2026-10-21. Versions below are evidence points, not
permission to add them without a fresh tidy and vulnerability review.

Transitive counts came from a new scratch module per candidate:

```sh
go mod init scratch/<candidate>
go get <module>@<version>
go mod graph
go list -m all
```

The count excludes the scratch main module but includes every module selected
by the graph, including test/tool requirements declared upstream. A second
scratch copy of this repository's `go.mod` and `go.sum` measured additions and
version changes against the current graph.

Maintenance evidence uses the upstream repository's latest release and push
dates. Licence evidence is the licence shipped at the reviewed tag. CVE evidence
uses the [OSV API](https://osv.dev/docs/#tag/api) for the exact module/version
and for the module's full history. An exact-version result of zero is only a
dated observation; `make vulncheck` remains authoritative on every change.

## Decision

| Dependency | Outcome | Licence and maintenance | Closure and CVE evidence | Network and hermetic-test decision |
|---|---|---|---|---|
| [`github.com/jackc/pgx/v5`](https://github.com/jackc/pgx), including `puddle/v2`, `pgpassfile`, and `pgservicefile` | **Accept for cloud packages only.** Use native pgx rather than `database/sql` for PostgreSQL protocol, pooling, cancellation, and transaction control. | MIT. `v5.11.0` was released 2026-09-07; upstream was active in October 2026. | 15-module scratch graph. Against this repository it adds only pgx, puddle, pgpassfile, and pgservicefile, with no selected-version changes. OSV: 0 records affect `v5.11.0`; 10 historical pgx records, including GO-2024-2567, GO-2024-2606, GO-2026-4771, GO-2026-4772, and GO-2026-5004. | pgx opens only the configured PostgreSQL endpoint. `pgpassfile` and `pgservicefile` read local configuration; they do not discover a service remotely. Tests use a fake conformance backend by default and a pinned PostgreSQL service container over TCP under `integration`. No production fallback to a default host is allowed. |
| [`github.com/Azure/azure-sdk-for-go/sdk/storage/azblob`](https://github.com/Azure/azure-sdk-for-go/tree/main/sdk/storage/azblob) | **Accept for cloud packages only.** Reuse the repository's existing `azcore` and `azidentity` stack. | MIT. `v1.8.2` was released 2026-10-01; the Azure SDK repository had same-day activity at review. | 89-module standalone graph. Against this repository it adds 40 modules and changes `klauspost/compress` 1.19.0→1.19.2 and `stoewer/go-strcase` 1.3.0→1.3.1. OSV: 0 exact or historical azblob records. The implementation PR must explain the unusually broad graph and show `make tidy-check` and `make vulncheck`. | The blob endpoint is required operator configuration; code must not synthesize an Azure public endpoint or enable unconfigured exporters. Authentication uses the existing credential seam. Unit tests use an HTTP fake; integration tests use pinned Azurite over loopback/TCP with `--disableTelemetry`. Images are pulled by CI setup, never by Go test code. |
| [`github.com/aws/aws-sdk-go-v2/service/s3`](https://github.com/aws/aws-sdk-go-v2) | **Defer.** It is the reviewed candidate, not an approved dependency. Re-open only when a non-Azure deployment target is committed, as required by D-7. | Apache-2.0. `v1.114.3` was released 2026-10-09; upstream had same-day activity at review. | 11-module scratch graph and 11 additions against this repository. OSV: 0 records affect `v1.114.3`; two historical records, including GO-2026-5764. | The default AWS configuration chain can contact region/credential discovery services, and normal endpoint resolution can choose AWS service endpoints. A future decision must require an operator-configured S3 endpoint, explicit credentials/region, and disabled metadata/default discovery before satisfying SEC-048. MinIO tests remain deferred with this SDK. |
| Migration library | **Reject.** | No module is selected, so there is no licence, maintenance, closure, or CVE surface to accept. | Zero added modules. | Keep ordered, embedded SQL migrations under product control, following the existing read-model migration pattern. This avoids runtime migration discovery, plugin acquisition, and another schema-locking policy. |
| [`github.com/testcontainers/testcontainers-go`](https://github.com/testcontainers/testcontainers-go) | **Reject.** | MIT. `v0.44.0` was released 2026-08-07 and upstream remained active at review. | 79-module scratch graph. OSV: 0 exact or historical module records. | It talks to a container daemon, pulls images, and normally starts the Ryuk cleanup container. Those hidden acquisitions and daemon coupling conflict with hermetic unit tests and the explicit CI acquisition inventory. CI service containers are simpler, visible, and shared across tagged integration packages. |
| [`github.com/Shopify/toxiproxy/v2`](https://github.com/Shopify/toxiproxy) | **Reject as a Go dependency; allow a CI-acquired binary later.** | MIT. `v2.13.0` was released 2026-10-14 and upstream remained active at review. | 48-module scratch graph; `v2.13.0` also requires Go 1.27, newer than this repository's Go 1.26.10. OSV: 0 exact or historical module records. | The server and client communicate only on configured TCP/HTTP addresses and have no reviewed telemetry path. A later chaos-test PR may download a pinned, checksummed binary in a declared CI setup step, cache it, and run it only in tagged integration jobs. It must not enter `go.mod` or production images. |

The pgx companion modules are accepted as part of that one decision, but remain
independently reviewable:

| Module selected by the pgx graph | Licence and maintenance | CVE and behavior evidence |
|---|---|---|
| [`github.com/jackc/puddle/v2`](https://github.com/jackc/puddle) `v2.2.2` | MIT. Upstream released `v2.2.3` and was active on 2026-10-05. The implementation PR may take that patch only if pgx selects it and the graph review stays clean. | OSV reports no historical records. It is an in-process resource pool with no endpoint or telemetry behavior of its own. |
| [`github.com/jackc/pgpassfile`](https://github.com/jackc/pgpassfile) `v1.0.0` | MIT. Stable, narrow module; last repository activity at review was 2023-08-22. | OSV reports no historical records. It parses a local PostgreSQL password file and performs no network I/O. |
| [`github.com/jackc/pgservicefile`](https://github.com/jackc/pgservicefile) `v0.0.0-20240606120523-5a60cdf6a761` | MIT. Stable, narrow module; last repository activity at review was its 2024-06-06 revision. | OSV reports no historical records. It parses a local PostgreSQL service file and performs no network I/O. |

The azblob graph selected by the first tagged cloud integration test is broader
than its HTTP client surface. These modules are direct requirements of
`azblob@v1.8.2` (confirmed by `go mod graph`), reached through
`azblob/container/internal/arrow`; they are not unpruned test-only dependencies.
`make vulncheck` reports no reachable vulnerability in the selected graph, and
the `TestGoobersClosureExcludesCloudDeps` rule confirms none is reachable from
the local `goobers` binary.

| Module selected directly by azblob | Licence | Closure and phone-home evidence |
|---|---|---|
| [`github.com/apache/arrow-go/v18`](https://github.com/apache/arrow-go) `v18.7.0` | Apache-2.0 | Supplies Arrow arrays and IPC encoding used by azblob's container query path. It is an in-process data-format library and opens no endpoint or telemetry channel. |
| [`github.com/google/flatbuffers`](https://github.com/google/flatbuffers) `v25.12.19+incompatible` | Apache-2.0 | Serialization runtime reached through Arrow IPC. It operates only on caller-provided memory/streams and performs no discovery, network access, or telemetry. |
| [`github.com/pierrec/lz4/v4`](https://github.com/pierrec/lz4) `v4.1.28` | BSD-3-Clause | In-process LZ4 codec reached through Arrow IPC. It has no network or telemetry behavior. |
| [`github.com/zeebo/xxh3`](https://github.com/zeebo/xxh3) `v1.1.0` | BSD-2-Clause | In-process hash implementation reached through Arrow arrays. It has no network or telemetry behavior. |
| [`github.com/klauspost/compress`](https://github.com/klauspost/compress) `v1.19.2` | BSD-3-Clause | In-process compression codecs. The module was already reachable from the local operator metrics stack; azblob raises the selected version from 1.19.0 to 1.19.2 but does not add a network path. |
| [`github.com/klauspost/cpuid/v2`](https://github.com/klauspost/cpuid) `v2.4.0` | MIT | Local CPU-feature detection used to select codec/hash implementations. It reads process/CPU state only and performs no network access or telemetry. |
| [`github.com/goccy/go-json`](https://github.com/goccy/go-json) `v0.10.6` | MIT | In-process JSON codec reached through Arrow. It performs no discovery, network access, or telemetry. |

## Integration service tooling

These are test processes, not Go module dependencies:

| Service | Decision | Licence, maintenance, CVE posture | Hermetic and phone-home requirements |
|---|---|---|---|
| PostgreSQL | **Accept** as the database integration service. | PostgreSQL Licence; maintained upstream. Select a supported major and immutable image digest when the first store test lands. Container CVEs are digest-specific, so scan and record that digest in the implementation PR rather than claiming this record covers a future image. | CI provisions the service before `go test`; tests receive an explicit TCP DSN. No socket auto-discovery, Docker access, or image pull occurs in test code. Local offline runs use a pre-pulled identical digest or an explicitly supplied DSN. |
| [Azurite](https://github.com/Azure/Azurite) | **Accept** as the Blob integration service. | MIT. `v3.37.0` was released 2026-08-26 and upstream remained active at review. Pin and scan an immutable image digest when added. | Azurite collects telemetry by default, so every invocation must pass `--disableTelemetry`. Bind only the CI service network/loopback address and inject its Blob endpoint. Pre-pull the pinned digest for offline reproduction. |
| [MinIO](https://github.com/minio/minio) | **Defer with S3.** Do not add it merely to test an uncommitted backend. | AGPL-3.0. The community repository is archived and its latest GitHub release at review was `RELEASE.2025-10-15T17-29-55Z`; maintenance and redistribution posture therefore require a fresh review if D-7 changes. | A future review must pin and scan an image, disable update/telemetry behavior by verified configuration, use explicit test credentials and endpoint, and prove operation with external egress denied. |
| Toxiproxy | **Allow only as described above:** a pinned CI binary, not a module or shipped runtime dependency. | MIT; exact binary version and checksum are selected and scanned by the chaos-test PR. | Acquisition is a named, cached CI step with a local preprovisioned path. Tests connect to an explicit loopback/service address and never download the binary. |

## Enforcement and implementation checklist

The first implementation PR for each accepted dependency must:

1. keep imports below a cloud-only package root and make
   `TestGoobersClosureExcludesCloudDeps` pass;
2. show the before/after selected-module closure for `./cmd/goobers` and prove
   no cloud module or module-version change entered it, using:

   ```sh
   go list -deps -f '{{with .Module}}{{.Path}} {{.Version}}{{end}}' \
     ./cmd/goobers | sort -u
   ```

3. run `make tidy-check`, `make vulncheck`, licence review, and no-phone-home;
4. keep ordinary unit tests offline and container-free;
5. put service-container tests behind `//go:build integration`, with explicit
   TCP endpoints, immutable image digests, bounded readiness, and cleanup;
6. update the runtime-acquisition manifest for every new hosted-CI download or
   image pull, including its offline preparation path.

This issue adds no module to `go.mod` and no dependency allowlist. The import
allowlist belongs to #7714 and must encode this record's cloud-only boundary.

## Consequences and residual risks

- D-4 is confirmed: `goobers` remains the local binary and `goobers-cloud`
  receives the accepted cloud dependencies.
- D-7 is confirmed: Azure Blob Storage is the only object-store implementation
  in the committed programme; S3 and MinIO remain absent.
- pgx adds a small, focused graph. Azblob's graph is much broader than its API
  surface suggests and can move existing versions, so accepting it does not
  waive per-upgrade graph review.
- Service emulators are not production-equivalence proof. Conformance must also
  run against the production service in an explicitly provisioned, non-merge
  environment before a backend is certified.
- CVE and maintenance findings are snapshots. Dependabot/module updates,
  `make vulncheck`, image scanning, and immutable pins carry the ongoing duty.

Closes #7711.
