# Reviewer dispositions

Reviewer decisions distinguish changes to make, waiting for siblings, terminal
rejection, and a mechanical stop. The original rationale and findings remain
part of the verdict artifact and published provider comment; operator run
summaries expose `reasonCode`, `findings`, and `legacyFailAmbiguous`.

| Decision | Meaning | Reason codes |
|---|---|---|
| `pass` | Review permits the next configured step; other merge checks still apply | None |
| `needs-changes` | Repairable implementation findings | None |
| `fail` | Terminal implementation or policy rejection | `implementation-rejected`, `policy-rejected` |
| `defer` | Withhold landing authority for sibling ordering | `ordering`, `no-lander` |
| `escalate` | Mechanical stop, not substantive rejection | `empty-diff`, `unchanged-repass`, `repass-budget-exhausted`, `finding-set-oscillation` |

Typed rejection, deferral, and escalation require a non-empty rationale.
Deferrals and mechanical stops cannot claim `elected: true`.

## Workflow opt-in

DSL 3.0 is preview. An agentic gate declares a `defer` branch to opt into the
expanded reviewer vocabulary. The runner derives the prompt capability from
that branch, not task inputs. Declaring both `defer` and `escalate` also opts
the gate into typed runner-generated empty-diff, unchanged-repass, and
exhausted-repass-budget stops. Route those branches to distinct tasks as needed:

```yaml
branches:
  pass: publish
  needs-changes: implement
  fail: record-rejection
  defer: park-ordering
  escalate: record-mechanical-stop
```

These are branch targets, not built-in task names: the workflow must define
each referenced task. Existing three-outcome gates keep their original prompt
contract and synthesized stop vocabulary. Unexpected deferral from a producer
without a declared route fails closed. The frozen DSL 2.0 interpreter does not
accept the new deferral branch.

## PR publication and election

`apply-verdict` turns ordering-only needs-changes into `defer / ordering` after
single-lander election; actual repairable findings still remain needs-changes.
A no-lander result becomes `defer / no-lander`, not an implementation rejection.
The provider-native review is comment-only for deferral and mechanical stops.
No-lander deferrals do not queue a supposedly crowned lander.

The election reader excludes a no-lander candidate only when the trusted
canonical verdict matches the current head and base. A changed pin permits
re-evaluation; an untrusted comment cannot exclude a candidate. Neither kind
of deferral satisfies the SHA-pinned pass check required to merge.

The publication result's `reason` output carries the structured reason for
downstream routing. Finding-set oscillation is published as mechanical
escalation with its original findings, not as substantive `fail`.

## Legacy verdicts

Old producers may still emit `fail` without a reason. Such verdicts remain
readable, but provider comments and publication results identify them as
`legacy-fail-ambiguous`; operator summaries set `legacyFailAmbiguous: true`.
Do not infer implementation rejection from that legacy decision alone.
The original verdict is not rewritten to invent a rejection reason.
