# #4199: merge-review verdict as a native merge-blocking status

`apply-verdict` and `merge-pr` are `cmd/goobers` stage commands, and the
verdict has to reach the provider's merge policy from inside them. The growth
is the wiring at those call sites:

- `apply-verdict` publishes the verdict status right after the sticky verdict
  comment (`verdictRecord`). It withdraws a standing status when a review is
  void (`withdrawStandingVerdict`) and builds the review identity that
  publishes (`newVerdictStatusPublisher`).
- The Azure DevOps verdict publishers pin the non-pass status to the reviewed
  head and record the base.
- `merge-pr` adds one conjunct, and both commands' help text describes it.

The reusable parts live outside the command package. The status contract,
the description format with the recorded base, the decision-to-state mapping,
the publish-or-degrade policy and the merge-pr refusal rule are in
`internal/prstatus`. The GitHub commit-status publisher, the CI and verdict
split in the poll, and the permission-error classifier are in `providers`.
`runApplyVerdict` and `runMergePR` stay within their complexity and
body-length baselines.
