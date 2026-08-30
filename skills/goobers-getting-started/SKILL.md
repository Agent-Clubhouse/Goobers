---
name: goobers-getting-started
description: Inspect a target repository and create the smallest safe, release-matched Goobers configuration while asking only for choices that cannot be derived.
---

# Goobers Getting Started

Create a repository-aware Goobers configuration source without making the user
translate repository structure into Goobers concepts.

## Safety boundary

During discovery, use only read-only filesystem, Git, provider, and Goobers
commands. Do not execute target-repository scripts, install dependencies, clone
repositories, change provider state, or read credential values. Configuration
generation may write only to the user-selected configuration source after the
proposed files, state graph, and capabilities have been explained.

## Procedure

1. Load `goobers-environment-resolver` and resolve the selected Goobers release,
   target repository identity, default branch, configuration source, and
   available release-matched contracts.
2. Load `goobers-dsl-author` and use its prospective-target repository
   inspection. Reuse its evidence ledger, provider-identity handling, schemas,
   capability registry, authoring procedure, and validation loop rather than
   duplicating them here.
3. Derive the target's authoritative aggregate local CI command and required
   toolchain from repository guidance, CI definitions, task-runner files,
   manifests, and lockfiles. Never run the command during discovery.
4. Ask only when provider identity, default branch, or CI evidence remains
   ambiguous, or when requested behavior requires an explicit choice about
   agent harness, mutation authority, scheduling, merging, or escalation.
5. Default to the smallest safe configuration that satisfies the request:
   - one gaggle targeting the evidenced repository and default branch;
   - one manual workflow;
   - deterministic local CI using the evidenced aggregate command;
   - evidenced toolchain requirements;
   - `maxConcurrentRuns: 1`;
   - no agentic goober, write capability, schedule, merge, or provider mutation
     unless the user requested that behavior.
6. Recommend tracking the configuration source in Git, but do not require a Git
   repository for a local-directory source.
7. Before writing, present the repository evidence, selected release and DSL
   version, proposed paths, state graph, and capability rationale.
8. Delegate generation, structured validation, and repair to
   `goobers-dsl-author`. Return its final diff and `ready` or `unresolved`
   result. Do not report success until release-matched validation passes.

## Starter prompt

Use the Goobers Getting Started skill to inspect target repository
`<target-path-or-provider-url>`, derive its default branch, CI command,
toolchain, and conventions, and create the smallest validated read-only config
source at `<config-path>`. Ask only when required evidence or behavior cannot be
safely derived.
