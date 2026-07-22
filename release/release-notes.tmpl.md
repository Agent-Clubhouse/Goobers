# Goobers {{.Version}}

## Highlights

<!-- Replace this placeholder with curated highlights before publishing. -->

- No curated highlights supplied.

## DSL feature-support delta

{{if .PreviousRelease}}Compared with `{{.PreviousRelease}}`.{{else}}This is the first recorded DSL feature-registry snapshot; GA, deprecated, and removed features are reported from an empty baseline.{{end}}

### Newly GA

{{range .Delta.NewlyGA}}- `{{.ID}}` (GA since `{{.SinceVersion}}`)
{{else}}- None.
{{end}}
### Newly deprecated

{{range .Delta.NewlyDeprecated}}- `{{.ID}}` (deprecated since `{{.SinceVersion}}`)
{{else}}- None.
{{end}}
### Removed

{{range .Delta.Removed}}- `{{.ID}}` (removed since `{{.SinceVersion}}`)
{{else}}- None.
{{end}}
The complete DSL feature-registry snapshot shipped by this release is attached as `feature-registry.json`.

## Support policy for external consumers

- Pin the Goobers binary release and retain its `feature-registry.json`; that snapshot is the authority for the DSL features the binary supports.
- Within an `apiVersion`, adding optional fields, enum values, or stage/gate kinds, relaxing constraints, and promoting preview features to GA are non-breaking changes.
- Removing or renaming fields, tightening constraints, changing defaults, or changing semantics is breaking. Such changes require a deprecated-to-removed cycle spanning at least one released minor version, or an `apiVersion` bump.
- Preview features are usable but unstable and carry no compatibility guarantee. GA features are supported without an opt-in.
- Deprecated features remain accepted with warnings through the deprecation window. Removed features are rejected by validation.
