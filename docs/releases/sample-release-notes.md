<!-- Illustrative generated output; this is not a published release. -->

# Goobers v0.2.0

## Highlights

<!-- Replace this placeholder with curated highlights before publishing. -->

- No curated highlights supplied.

## DSL feature-support delta

Compared with `v0.1.0`.

### Newly GA

- `trigger.schedule` (GA since `v0.2.0`)

### Newly deprecated

- `stage.shell` (deprecated since `v0.2.0`)

### Removed

- `trigger.webhook` (removed since `v0.2.0`)

The complete DSL feature-registry snapshot shipped by this release is attached as `feature-registry.json`.

## Support policy for external consumers

- Pin the Goobers binary release and retain its `feature-registry.json`; that snapshot is the authority for the DSL features the binary supports.
- Within an `apiVersion`, adding optional fields, enum values, or stage/gate kinds, relaxing constraints, and promoting preview features to GA are non-breaking changes.
- Removing or renaming fields, tightening constraints, changing defaults, or changing semantics is breaking. Such changes require a deprecated-to-removed cycle spanning at least one released minor version, or an `apiVersion` bump.
- Preview features are usable but unstable and carry no compatibility guarantee. GA features are supported without an opt-in.
- Deprecated features remain accepted with warnings through the deprecation window. Removed features are rejected by validation.
