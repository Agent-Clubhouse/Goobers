---
name: ci-requires-node-20
description: "The CI toolchain requires Node 20; older versions fail the build."
type: environment
scope:
  areas: []
  workflows: []
  roles: []
  labels: ["ci", "build", "environment"]
provenance:
  source: human
  proposedBy: coder
  promotedBy: wizard
  promotedAt: 2026-01-12T05:00:00Z
confidence: proven
reviewAfter: ""
supersedes: []
---
# Environment: CI requires Node 20

## Fact
The build and CI suite require Node 20. A runner on an older Node fails during
dependency install.

## Evidence
Builds on Node 18 failed at install with an engine mismatch. Node 20 builds
cleanly. The gaggle declares `node@20` as a runner capability.

## Do instead
Assume Node 20 in build scripts and CI config. Do not add code paths or tooling
that depend on a Node version the runners do not advertise.
