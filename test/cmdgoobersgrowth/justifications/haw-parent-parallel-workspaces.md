# Contained parent parallel workspace admission

The command policy now admits seeded, contained agentic parallel stages after
the core runner provides isolated forks and branch-aware waiting. Workspace
selection, durable fork plans, replay, archive verification and retirement live
in internal/runner, internal/recovery and internal/worktree. The command retains
only backend policy and the real factory composition test.

Existing repoFrom semantics select the latest declared producer that finished;
the join does not merge all independent branch trees. Unselected contributions
remain digest-addressed journal artifacts after checkout retirement for explicit
later review or reconciliation. A parallel as the start state remains refused
until its common base has a durable pin; a repository-producing seed is required.
