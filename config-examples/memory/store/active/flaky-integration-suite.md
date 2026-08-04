---
name: flaky-integration-suite
description: "The integration suite is flaky under parallel execution."
type: fragility
scope:
  areas: ["src/integration/**"]
  workflows: ["implementation"]
  roles: []
  labels: ["ci", "test"]
provenance:
  source: human
  proposedBy: reviewer
  promotedBy: wizard
  promotedAt: 2026-01-14T05:00:12Z
confidence: observed-once
reviewAfter: "2026-04-01"
supersedes: []
---
# Flaky integration suite under parallel execution

## Fact
The integration suite intermittently fails when run with more than one worker.

## Evidence
Observed across multiple runs; every failure was on the parallel path and a
serial rerun passed. Root cause is an unsynchronized shared fixture.

## Do instead
Pin the integration suite to a single worker until the shared-fixture race is
fixed. Do not chase the individual test failures as if they were real.

Related: [[shared-fixture-race]]
