# Investigation artifacts

Agentic stages can publish a bounded set of evidence files using
`inputs.artifactManifestFile`. This is mutually exclusive with `artifactFile`.
The completion envelope must leave `artifacts` empty; the runner authors the
published pointers.

Write the payload files into the stage workspace, then use `publish_output` to
write the staging manifest to the declared manifest path:

```json
{
  "schemaVersion": "goobers.dev/stage-artifact-set/v1alpha1",
  "entries": [
    {"name": "diagnosis.report", "path": "diagnosis.json", "mediaType": "application/json"},
    {"name": "runtime.trace", "path": "capture.trace", "mediaType": "application/x-go-trace"}
  ]
}
```

Paths must be workspace-relative regular files. Escapes through paths or
symlinks, special files, duplicate names, malformed JSON, and unsafe payloads
are rejected. The whole set is validated and scrubbed before publication.
The staging manifest itself is not published. The runner sorts semantic names,
records payloads in slots 1 through N, and publishes a normalized index at slot
0. Consumers resolve names through that index and verify all indexed pointers
and payload digests; semantic names are not `ContextPointer.Name` aliases.

## Formats and limits

A set permits at most 64 entries, 16 MiB per payload, and 64 MiB total, checked
both before and after sanitization. The staging manifest and normalized index
are each bounded to 128 KiB. These are hard limits, not truncation targets.

Supported media types are plain text, Markdown, patches/diffs (`text/plain`,
`text/markdown`, `text/x-patch`, `text/x-diff`), JSON (`application/json`), tar
(`application/x-tar`), gzip-compressed tar (`application/gzip`), native Go
execution traces (`application/x-go-trace`), and gzip-compressed pprof protobuf
profiles (`application/x-pprof`). A goroutine dump can use `text/plain`.

JSON strings are decoded before secret scrubbing. Secret-bearing JSON keys,
duplicate keys, or nesting beyond 16 levels are rejected. Reproduction archives
permit at most 256 regular-file/directory entries; links, special files and
binary members are rejected. Archives are decompressed before scrubbing and
rebuilt without host metadata, subject to the same expanded byte bound.

Native traces support the Go 1.22, 1.23, 1.25 and 1.26 wire formats; Go 1.24 uses
the 1.23 wire format. They are parsed and capped at 1,048,576 emitted events.
Profiles have a 262,144-element protobuf preflight budget, a bounded single gzip
member, and schema/structural checks. Unknown profile fields are rejected.
Native diagnostic bytes are preserved only when the configured scrubber finds
nothing requiring redaction; otherwise publication fails. Binary strings are
never replaced in place. Profile gzip metadata is removed. Other opaque formats
are unsupported; do not disguise them with a text media type.

## Canonical investigation evidence

The evidence-packager writes an `application/json` payload named
`investigation.evidence` in its staging manifest. Its input document uses
`schemaVersion: goobers.dev/investigation-evidence-draft/v1alpha1` and follows
[the draft schema](../../api/schemas/investigation-evidence-draft.schema.json).
It contains the subject, environment, reproduction, diagnosis, fix, validation,
and optional attachments described in
[the investigation design](../design/deep-investigation-workflow.md#6-evidence-artifact-schema).

Every artifact position uses exactly a semantic reference, for example:

```json
{"producerStage": "reproduce", "name": "reproduction.bundle"}
```

This includes `reproduction.harness`, `reproduction.baseline`,
`diagnosis.report`, each `diagnosis.evidence[].artifact`, `fix.report`,
`validation.result`, and each `attachments[].artifact`. Do not copy or predict
artifact paths, digests, sizes, or slots in these positions. Scalar comparison
digests remain ordinary fields supplied by the upstream stages.

The runner validates the draft before reading artifacts, resolves references
through the four producing stages' complete normalized indices, and verifies
the current-run pointers and bytes. It then emits
`goobers.dev/investigation-evidence/v1alpha1` canonical JSON. Agent-authored
canonical evidence documents are rejected. Both draft and canonical evidence
are limited to 256 KiB. A failure anywhere in the declared artifact set prevents
publication of the prepared evidence manifest.

The manifest references existing artifacts without copying their payloads.
Schema validity does not attest that a fix works: the workflow's reproduction,
revision and final-ref gates must establish those facts separately.

## Retention

Journal-local payloads and normalized indices share their run's ordinary
telemetry retention policy. Expired terminal runs are pruned as a unit; running
and retained recent runs remain readable. Blobs left inside a run by an
interrupted publication follow that same policy.

This journal retention rule is not a fleet-store garbage collector. Shared
content-addressed stores and worker staging caches have separate lifecycles;
do not delete shared digests solely because one run has expired.
