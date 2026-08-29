# Flake management

Goobers treats intermittent test failures as tracked defects, not as permission
to weaken CI. The standing rule is **zero anonymous retries**: GitHub Actions
workflows must not automatically retry a failed step or job.

## Lifecycle

1. **Find:** the trusted scheduled or manually dispatched `Stress` workflow runs
   the enrolled packages with `-race -count=20`. A pull request carrying
   `/stress` can produce the same artifact, but untrusted pull-request code never
   receives issue-write permission.
2. **Fingerprint:** `test/stress` combines package, test name, and a normalized
   failure signature. Volatile durations, addresses, timestamps, UUIDs, and
   goroutine IDs do not split one defect into multiple identities; distinct
   assertions, panic sites, and data-race sites in the same test do.
3. **Record:** `test/flakeledger` ensures the `ci:flake` label exists. A new
   fingerprint files one issue with its identity, occurrence, run link, and
   failure snippet. Later occurrences append to that issue. These issues have no
   milestone; the publisher applies `ci:flake` and removes any `goobers:*` or
   `goobers/status:*` workflow labels before refreshing an issue so flakes cannot
   enter an automated implementation queue accidentally.
   The hourly `Flake watch` workflow also scans failed checks on open pull
   requests and recent default-branch runs. It prefers structured check
   annotations and temporarily falls back to Actions job logs until structured
   test artifacts are available. Exact or signature-equivalent ledger matches
   dispatch a `flake-fixer` repository event immediately. Novel timing,
   contention, ordering, timeout, and race candidates flow through the same
   ledger publisher; PR failures whose annotation path is changed by that PR
   remain ordinary regressions.
4. **Quarantine when necessary:** use the only supported skip form:

   ```go
   flake.Quarantine(t, "#1234", "2026-08-15")
   ```

   The reference must be `#<issue>` or a full GitHub issue URL. The date is
   interpreted in UTC and permits the skip through that day. On the next day,
   the helper fails the test. A quarantine extension therefore requires an
   explicit code change tied to the issue; raw `t.Skip` calls for flaky,
   intermittent, unstable, or quarantined behavior are rejected.
5. **Fix:** remove the quarantine, exercise the affected package through the
   stress workflow, document the confirming run on the flake issue, and close
   the issue. If the fingerprint recurs later, the ledger appends the new
   occurrence to the existing issue.

`make flake-policy` enforces the two repository-level rules: flake-worded raw
test skips are forbidden outside the quarantine helper, and workflow files may
not contain retry/rerun configuration. It is also part of `make ci`.

## Rerunning a failure

Do not add a retry action, failure loop, or automatic `gh run rerun` command to
a workflow. First inspect the failure. If it matches an existing fingerprint,
record the run on that flake issue; if it is a candidate new flake discovered
from a pull request, confirm it with a trusted manual stress dispatch so the
ledger can file it. A human may then rerun manually, with the reason and flake
issue link recorded on the issue. A failure without that identity remains a
real failure.
