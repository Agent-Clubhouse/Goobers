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

{{.SupportDelta}}

## Support policy for external consumers

- Pin the Goobers binary release and retain its `feature-registry.json` and `dsl-support-matrix.json`; those snapshots are the authority for the DSL features and DSL versions the binary supports.
- In the support matrix, `level` describes this binary's behavior. Optional `effectiveIn` records actual enforcement when it differs from the retained policy history; DSL 1.4's `v0.4.0` value discloses its already-shipped early removal against the prior `v0.5.0` promise.
- Within an `apiVersion`, adding optional fields, enum values, or stage/gate kinds, relaxing constraints, and promoting preview features to GA are non-breaking changes.
- Removing or renaming fields, tightening constraints, changing defaults, or changing semantics is breaking. Such changes require a deprecated-to-removed cycle spanning at least one released minor version, or an `apiVersion` bump.
- Preview features are usable but unstable and carry no compatibility guarantee. GA features are supported without an opt-in.
- Deprecated features remain accepted with warnings through the deprecation window. Removed features are rejected by validation.
{{if .Prerelease}}
## Installing this pre-release

`install.sh` intentionally accepts stable tags only. Download the archive and
`SHA256SUMS` from this release, verify before extraction, and keep the extracted
documentation beside the binary. Linux/macOS:

```sh
version={{.Version}}
case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) exit 1 ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) exit 1 ;; esac
archive="goobers_${version}_${os}_${arch}.tar.gz"
url="https://github.com/Agent-Clubhouse/Goobers/releases/download/${version}"
curl -fsSLO "${url}/${archive}" && curl -fsSLO "${url}/SHA256SUMS" || exit 1
awk -v name="$archive" '$2 == name { print; found=1 } END { if (!found) exit 1 }' SHA256SUMS > selected-checksum || exit 1
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum --check selected-checksum || exit 1
else
  shasum -a 256 --check selected-checksum || exit 1
fi
mkdir "goobers-${version}" && tar -xzf "$archive" -C "goobers-${version}" || exit 1
"./goobers-${version}/goobers" --version
```

On Windows, download `goobers_{{.Version}}_windows_amd64.zip` and `SHA256SUMS`,
compare `Get-FileHash -Algorithm SHA256` against its manifest entry, then use
`Expand-Archive` and run the extracted `goobers.exe --version`.
{{end -}}
