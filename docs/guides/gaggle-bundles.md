# Portable gaggle bundles

`GaggleBundle` is the versioned public contract for exporting one gaggle and
creating an independent copy in another Goobers instance. The source is always
read-only. Import validates the complete bundle and destination configuration
before atomically installing the new gaggle.

The canonical JSON Schema is
[`api/schemas/gaggle-bundle.schema.json`](../../api/schemas/gaggle-bundle.schema.json).
The Go API types are in `api/v1alpha1/gaggle_bundle_types.go`.

## Export

```text
goobers gaggle export --output example.bundle.json example ./instance
```

The daemon also exposes an authorized read route:

```text
GET /api/v1/gaggles/{gaggle}/bundle
```

The bundle contains the canonical `Gaggle`, `Workflow`, and `Goober` API
objects, referenced Goober instruction files, referenced skill package files,
logical repository identities, source identity/API version/digest, an
export timestamp, and exporter provenance.

The bundle digest is `sha256:` plus the SHA-256 of the canonical JSON encoding
of `definition`. Re-exporting unchanged sanitized definitions produces the same
digest even though `exportedAt` changes. Workflows, Goobers, files, and
repository references are sorted before hashing.

Export normalizes or omits destination-specific fields and lists every omitted
field class in `provenance.sanitizedFields`. Bundles never include:

- instance credentials, tokens, secret values, or repository authorization;
- resolved or explicitly configured task environment values;
- provider self identity, host/pairing identity, or workload identity;
- runs, attempts, journals, logs, transcripts, or other runtime state;
- local workcopy roots, outbox mirror roots, or other absolute host paths;
- manifest connections or connection credential references.

Repository entries are credential-free logical identities only. They are not
proof of access and cannot authorize the destination.

Export fails explicitly instead of silently dropping task `run.env` values or
opaque Goober `harnessOptions`, because their contents cannot be proven
portable and credential-free.

## Import

```text
goobers gaggle import --name copied-example example.bundle.json ./instance
```

The daemon mutation route is:

```text
POST /api/v1/gaggles/import
Content-Type: application/json

{
  "name": "copied-example",
  "bundle": { "...": "complete GaggleBundle" }
}
```

Import requires the destination `instance.yaml` to already contain a matching
authorized `repos[]` entry for every logical repository in the bundle. It never
copies credentials or connection authorization from the source.

Before mutation, import verifies the envelope version, complete digest, file
digests and contained paths, gaggle/workflow/Goober references, destination
name, destination repository authorization, and the fully materialized
candidate with the existing configuration validator. It then uses the existing
configuration lock and atomic directory-swap mechanism. Invalid schema, digest,
reference, name conflict, missing authorization, validation failure, write
failure, or daemon reload rejection returns an explicit error and leaves no
partial gaggle.

The destination receives a new local gaggle name and destination-derived
isolation namespace. `bundle-source.json` records the retained public source
identity, digest, export timestamp, import timestamp, and exporter provenance.
