# Coordinate a shared backlog with assignees

Assignment-aware coordination lets multiple Goobers instances, or a mixed
human-and-Goobers team, divide one provider backlog using its native assignee
field. It has three independent, **opt-in** parts:

1. `backlog-query` can limit dispatch to one assignee.
2. The `backlog-assignment` workflow can assign eligible, unassigned items from
   a roster.
3. Items parked with `goobers:needs-human` can be assigned to a designated
   human.

All three default off. Existing instances that omit these settings continue to
ignore assignees during backlog selection, do not run an assignment workflow,
and only label needs-human items. Assignment-aware configuration is not
required for a single-instance gaggle.

## Configure the instance identity

`selfIdentity` is a provider login, not a credential. Set it at the top level of
`instance.yaml` to provide a default for every gaggle in that instance:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Instance
selfIdentity: acme-goober-east
repos:
  # ...
```

A gaggle can override that default with `spec.selfIdentity`:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: payments
spec:
  selfIdentity: acme-payments-bot
  # project, backlog, and isolation ...
```

Use the GitHub, Gitea, or Azure DevOps identity that provider work items are
actually assigned to. This setting does not select credentials or change the
identity used to authenticate provider calls.

## Restrict dispatch to that identity

Opt an `implementation` or `backlog-curation` workflow into assignee filtering
on each `goobers backlog-query` task:

```yaml
- name: query-backlog
  type: deterministic
  run:
    command: ["goobers", "backlog-query", "--claim"]
  inputs:
    trustLabel: "goobers:approved"
    requireLabels: "goobers:ready"
    excludeLabels: "goobers/status:in-review"
    respectAssignee: "true"
    resultFile: "claimed-item.json"
```

Leave `assignedTo` undeclared, as above, to use the gaggle's effective
`selfIdentity` (the gaggle override, then the instance default). The runner only
injects this default into `goobers backlog-query` tasks, and an explicit task
input always wins.

The two inputs have these exact semantics:

| Configuration | Eligible items |
|---|---|
| `respectAssignee` omitted or not `"true"` | Assignee is ignored; this is the backward-compatible default |
| `respectAssignee: "true"` and `assignedTo` omitted | Only items assigned to the effective `selfIdentity` |
| `respectAssignee: "true"` and `assignedTo: "alice"` | Only items assigned to `alice` |
| `respectAssignee: "true"` and `assignedTo: ""` | Only unassigned items |

When filtering for an identity, items assigned to another identity and
unassigned items are ineligible. Configure a non-empty `selfIdentity`, or
declare `assignedTo` explicitly, before enabling identity-based dispatch.

## Add the assignment workflow

Copy
[`config-examples/gaggles/acme-web/workflows/backlog-assignment.yaml`](../../config-examples/gaggles/acme-web/workflows/backlog-assignment.yaml)
into the gaggle that will coordinate assignment. Merely shipping or copying the
definition does not enable it; the workflow runs only where it is installed
with a trigger.

The deterministic task accepts:

| Input | Meaning |
|---|---|
| `strategy` | Required: `"constant-cap"` or `"round-robin"` |
| `roster` | Required non-empty JSON array of assignee entries |
| `trustLabel` | Required trust boundary; only items carrying this label are considered |
| `requireLabels`, `excludeLabels`, `labelPredicate`, `fieldPredicate` | Optional filters defining the backlog slice |
| `maxItems` | Maximum assignments per pass; defaults to `20` |
| `resultFile` | Report path; defaults to `backlog-assignment.json` |

The task must declare the provider issue-write capability and `update-issue`
policy action:

```yaml
- name: assign-backlog
  type: deterministic
  run:
    command: ["goobers", "backlog-assignment"]
  inputs:
    strategy: "round-robin"
    roster: '[{"assignee":"acme-goober-east"},{"assignee":"acme-goober-west"}]'
    trustLabel: "goobers:approved"
    requireLabels: "goobers:ready"
    excludeLabels: "goobers/status:in-review"
    maxItems: "20"
    resultFile: "backlog-assignment.json"
  capabilities:
    - github:issues:write
  policyActions:
    - update-issue
  expectedOutputs:
    - backlog-assignment
```

`constant-cap` entries require a positive `maxOpen`:

```yaml
strategy: "constant-cap"
roster: '[{"assignee":"acme-goober-east","maxOpen":3},{"assignee":"alice","maxOpen":2}]'
```

It fills the first roster member up to their cap, then the next, and leaves
overflow unassigned when every member is at capacity. `round-robin` entries
must omit `maxOpen`; each next item goes to the roster member with the fewest
currently open assigned items, with roster order breaking ties. Both strategies
process eligible unassigned items oldest-first and count existing open
assignments when calculating load.

Invalid strategy, roster, or trust-label configuration fails before any
provider mutation. Keep one instance responsible for this scheduled workflow
for a shared backlog; every participating instance may still run its own
assignee-filtered implementation workflow.

## Route needs-human items

Set the top-level instance field `needsHumanAssignee` to a human provider login:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Instance
needsHumanAssignee: alice
repos:
  # ...
```

When Goobers parks a work item with `goobers:needs-human`, it also assigns the
item to that identity so it appears in the provider's assigned-to-me view.
Omitting the field preserves label-only behavior. This setting is independent
of `selfIdentity`, assignee-filtered dispatch, and `backlog-assignment`; it can
be adopted by itself. Mechanical `goobers:needs-remediation` and ordinary
blocked-on-sibling parking are not routed to this assignee.

## Worked example: two instances, one shared backlog

Acme runs two independent instances against `acme/payments`. East and west have
separate provider identities and branch namespaces, but both use the same
backlog labels. The east instance is the assignment coordinator.

East's `instance.yaml` includes:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Instance
selfIdentity: acme-goober-east
needsHumanAssignee: alice
repos:
  # connection definitions for acme/payments ...
```

West's `instance.yaml` differs only in its self identity and connections:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Instance
selfIdentity: acme-goober-west
needsHumanAssignee: alice
repos:
  # connection definitions for acme/payments ...
```

Their gaggle definitions target the same project and backlog. East uses:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: payments
spec:
  displayName: Payments east
  project:
    provider: github
    owner: acme
    name: payments
    branch: main
  backlog:
    provider: github
    project: acme/payments
    labels: [goobers]
  branchNamespace: "goobers-east/"
  isolation:
    namespace: payments-east
    identityRef: payments-east-identity
```

West uses the same `project` and `backlog`, with a distinct namespace:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: payments
spec:
  displayName: Payments west
  project:
    provider: github
    owner: acme
    name: payments
    branch: main
  backlog:
    provider: github
    project: acme/payments
    labels: [goobers]
  branchNamespace: "goobers-west/"
  isolation:
    namespace: payments-west
    identityRef: payments-west-identity
```

> **`connectionRef` is not a runtime credential selector.** The local runner
> resolves every access's token from `instance.yaml` `repos[]` by repository
> identity, never from the named Connection, so declaring one connection for
> the project and another for the backlog would not route the two accesses
> through two credentials. `goobers validate` reports `REF012` (#3296)
> wherever the field is declared, and the shipped configs leave it out
> for that reason. Scope the token itself in `instance.yaml` when a narrower
> one is wanted.

Both instances add `respectAssignee: "true"` to the `query-backlog` task shown
above and leave `assignedTo` undeclared. East therefore claims only
`acme-goober-east` items, west claims only `acme-goober-west` items, and neither
claims unassigned work.

Only east installs the scheduled `backlog-assignment` workflow:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: backlog-assignment
spec:
  gaggle: payments
  displayName: Payments backlog assignment
  triggers:
    - type: schedule
      schedule: "29 */6 * * *"
  readiness:
    maxConcurrentRuns: 1
    maxRunsPerHour: 1
  start: assign-backlog
  tasks:
    - name: assign-backlog
      type: deterministic
      run:
        command: ["goobers", "backlog-assignment"]
      inputs:
        strategy: "round-robin"
        roster: '[{"assignee":"acme-goober-east"},{"assignee":"acme-goober-west"}]'
        trustLabel: "goobers:approved"
        requireLabels: "goobers:ready"
        excludeLabels: "goobers/status:in-review"
        maxItems: "20"
        resultFile: "backlog-assignment.json"
      capabilities:
        - github:issues:write
      policyActions:
        - update-issue
      expectedOutputs:
        - backlog-assignment
```

Each pass balances trusted, ready, unassigned work against the two identities'
current open loads. Alice receives items only when a workflow parks them as
needs-human. For a mixed human-and-Goobers team, replace one roster login with
the human's login and omit that instance: assigned issues appear in the
human's native queue while the Goobers instance continues to claim only its
own identity.

Assignee filtering scopes backlog dispatch, not pull-request lifecycle. Keep
distinct `branchNamespace` values for multiple Goobers instances, as shown
above; see [Run multiple Goobers instances against one repo](multiple-instances-one-repo.md)
for that complementary boundary.
