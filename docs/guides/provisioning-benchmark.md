# Provisioning benchmark reports

The reporting-only `Large Repository Scale` workflow runs weekly and on
`workflow_dispatch`. It provisions the B0 synthetic large repository through
the pinned-workspace path on the fixed `ubuntu-24.04` runner class. It has no
pull-request trigger, timing budget, or required-check role: provisioning
latency is reported but never gates a merge. Harness or report-format failures
still fail the scheduled run.

Each successful run uploads the `provisioning-benchmark` artifact containing
`provisioning-benchmark.json` and the Markdown workflow summary. The summary
compares each available phase and total wall time with up to five recent
successful artifacts. Artifact retrieval failures are emitted as workflow
warnings and result in a shorter comparison window.

The JSON format follows the F3 test-timing convention: `schemaVersion`, a stable
`job`, `platform`, `architecture`, and decimal `elapsedSeconds` are top-level
fields. It adds the source `revision`, workflow `runId`, pinned runner metadata,
phase timings, and the unmodified B0 harness result:

```json
{
  "schemaVersion": 1,
  "job": "large-repo-provisioning",
  "platform": "linux",
  "architecture": "amd64",
  "elapsedSeconds": 412.3,
  "runId": "123456789",
  "revision": "0123456789abcdef",
  "runner": {
    "class": "ubuntu-24.04",
    "name": "GitHub Actions 1",
    "image": "ubuntu24",
    "cpuModel": "Example CPU",
    "logicalCPUs": 4,
    "memoryBytes": 17179869184
  },
  "phases": {
    "fixtureGenerationSeconds": 300.1,
    "initToFirstRunSeconds": 100.2,
    "secondRunSeconds": 12.0
  },
  "benchmark": {
    "schema": "goobers.bench-workcopy/v2"
  },
  "recentRuns": []
}
```

Phase properties with no measurement in the selected provisioning mode are
omitted. `recentRuns` records the run, revision, total, and phase values used by
the trend summary. Consumers must reject unsupported `schemaVersion` values.
Additive fields within a version are allowed.

Run the healthy small fixture locally with:

```console
make bench-workcopy
```

Override `BENCH_WORKCOPY_ARGS` for larger fixtures. The scheduled >=10 GiB
corpus remains `make bench-large-repo`; ordinary local development should use
the small target.
