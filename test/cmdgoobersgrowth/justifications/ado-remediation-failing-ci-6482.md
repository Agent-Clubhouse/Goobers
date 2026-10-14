# #6482 (ADO): pr-remediation discovers failing CI on Azure DevOps

Growth: +232 non-test lines and +1 file in `cmd/goobers` on top of #7392. Most
of it is doc comment. The new file is `cmd/goobers/adoremediationprobe.go`.

## Why the growth belongs in the command package

- **`adoremediationprobe.go`**. This is the Azure DevOps eligibility pass for
  `gather-pr-context`, a `cmd/goobers` stage command. It replaces the ADO
  adapter's inline `prepare` closure.
  - `adoRemediationCandidates`, `adoCIProbeNumbers` and
    `adoCIProbeCanChangeTier` choose which PRs to read CI for: the target
    alone on a targeted run, otherwise a bounded window of the probeable PRs
    no other run holds.
  - `adoCIProbeWindow` and `advanceADOCIProbeWindow` keep that window
    rotating across cycles through a persisted scheduler-state cursor. The
    cursor uses the same keyed-record helpers (`statejson.go`) and stage
    state seam (`openStageStateStore`) as the pr-remediation no-op guard in
    this package.
  - The probe itself is the existing `resolveADOSelectedCheckState`, which
    calls `ADOProvider.HasPullRequestCIFailures` in `providers`.
- **The ADO adapter wiring in `gatherprcontext.go`**, and the `resolveCheck`
  short-circuit so the selected PR's CI is not read twice in one cycle.

The new key family's name and pattern live in `internal/stateclient`, beside
every other scheduler-state key.

## Could any of it live elsewhere?

The window arithmetic (`adoCIProbeWindow`) is pure. It could move with the
rest of the candidate ranking if that becomes its own package (see
`pr-remediation-skip-parked-6482.md`). The cursor and the probe depend on the
stage's state seam and provider adapter, so they stay here.
