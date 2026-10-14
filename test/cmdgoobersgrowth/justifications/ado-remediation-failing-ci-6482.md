# #6482 (ADO): pr-remediation discovers failing CI on Azure DevOps

Growth: +72 non-test lines and +0 files in `cmd/goobers` on top of #7392, all in
`cmd/goobers/gatherprcontext.go`. Most of it is doc comment.

## Why the growth belongs in the command package

- **`adoRemediationCandidates` / `adoCIProbeCanChangeTier`**. These replace
  the ADO adapter's inline `prepare` closure for `gather-pr-context`, a
  `cmd/goobers` stage command, with an eligibility pass that also probes each
  promotable PR's CI state. The probe itself is the existing
  `resolveADOSelectedCheckState`, which calls `ADOProvider.HasPullRequestCIFailures`
  in `providers`.
- **The ADO `resolveCheck` short-circuit**. The selected PR's CI state is not
  re-read when the probe already found it failing.

## Could any of it live elsewhere?

The tier decision (`adoCIProbeCanChangeTier`) would move with the rest of the
candidate ranking if that ever becomes its own package (see
`pr-remediation-skip-parked-6482.md`). The probe loop depends on the stage's
provider adapter, so it stays with the adapter.
