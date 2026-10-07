# #6707: admit ci-check-failure on the telemetry aggregate plane

Growth: +33 non-test lines and +0 files in `cmd/goobers`. Roughly half is doc
comment; the rest is the daemon-side wiring that has to sit beside the plane's
existing derivation.

## Why the growth belongs in the command package

- **`cmd/goobers/telemetrydefectplane.go`** (alias table entry, threshold
  fold-in, `sanitizeCICheckSubject` and its branch in `redactFindingForPlane`).
  The daemon-side service shares `detectCandidateFindingsWithCausalCredit` with
  the local `telemetry-query` path on purpose, so the plane and the local CLI
  cannot drift into two answers. The alias table, `planeThresholds` and
  `redactFindingForPlane` are already here; the new family is one more row in
  each, plus a short bounded-subject sanitizer for external-provider check
  names.
- **`cmd/goobers/telemetryquery.go`** (one threshold forwarded to the plane, the
  `min-ci-check-failure-runs` refusal removed, help text). The CLI is the only
  caller of the plane client.
- **Comments** rewritten from "four, and only these four" to say the set was
  widened for #6707 (pod-placed `goobers/test-suite-quality`).

## Could any of it live elsewhere?

The sanitizer is generic enough to move next to
`telemetryclient.NormalizeErrorSignature`, and that is the natural home if more
families need subject bounding. With one caller it stays beside
`redactFindingForPlane`, which is the only place that decides what crosses the
boundary. The alias table and threshold fold-in belong with the service that
owns them.
