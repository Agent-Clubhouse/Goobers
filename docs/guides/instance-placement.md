# Choose where an instance and its config live

Goobers has two placement decisions that are easy to conflate:

1. The **instance root** holds runtime state. Keep it outside every target
   repository.
2. The **config source** holds desired state. It may be instance-local, a
   subtree of a target repository, or a separate repository.

When people describe running Goobers "inside" a repository, they should mean
the second option: the config source is a non-empty subtree of that repository.
The instance root itself is still outside. For a third-party target or a
team-operated deployment, prefer a separate config repository.

## Keep runtime state outside target repositories

An instance root contains mutable execution data:

```text
<instance-root>/
  instance.yaml
  config/                         # active, materialized definitions
  gaggles/<gaggle>/
    runs/                         # journals, inputs, artifacts, transcripts
    workcopies/                   # managed clones and per-run worktrees
  scheduler/                      # decisions and claim ledger
  telemetry.db
```

Do not put this root in a target repository, even when the config source is in
that repository. Runtime state is generated operational evidence, not desired
state: it changes continuously, can be large, contains managed copies of target
content, and must never enter config review or a commit. Separating it also
prevents a run worktree from becoming nested inside the repository it operates
on.

Use a short, stable path with enough free space. For example:

```text
# POSIX
~/goobers/instances/widget/

# Windows
C:\goobers\widget\
```

A short path gives Windows workcopies and per-run worktrees more path-length
headroom. Enable Windows and Git long-path support as described in the
[Windows quickstart](quickstart-windows.md), but do not use that support as a
reason to bury the instance under a deep source checkout.

When journals and other runtime state should remain at the instance root,
redirect only managed working copies with an absolute base path:

```yaml
workcopies:
  root: C:\g
```

Goobers appends the gaggle name and repository key beneath this base. A gaggle
may override the instance default in its own `spec.workcopies.root`; this keeps
separate gaggles isolated even when they select the same short base path.
`goobers validate` rejects a relative root, and rejects two gaggles whose roots
resolve to the same directory or nest one inside the other, rather than letting
the daemon fail at startup.

Changing either root does not move existing mirrors or worktrees; Goobers
clones clean copies at the new location. Stop Goobers, confirm that no run is
active, and move the old workcopies directory aside as a temporary backup (or
remove it immediately). After updating the configuration and restarting, verify
the new checkouts, then remove the backup. Do not copy managed checkouts into
the new root because Git worktree metadata may contain the old absolute path.
Leaving the old directory in place is safe but continues to consume disk space.
For a complete host cutover, including journals, scheduler state, telemetry,
credentials, and split-brain prevention, follow
[Move a local instance to another machine](move-local-instance.md).

Before creating a checkout, Goobers measures the deepest tracked path and
refuses it when the worktree prefix plus that path exceeds the repository's
budget. Windows defaults to the 260-character `MAX_PATH` ceiling. Configure a
different ceiling and reserve room for generated build output per repository:

```yaml
repos:
  - provider: github
    owner: acme
    name: widget
    pathLength:
      maxPathLength: 320
      buildOutputAllowance: 48
```

Declaring `pathLength` also enables the check on non-Windows hosts. Set
`pathLength.disabled: true` to opt a repository out explicitly.

Prefer a parent directory that is not tracked by Git. If an operational parent
directory is itself version-controlled, but is not a target repository, ignore
the exact local-state directory at that repository's root:

```gitignore
/local-instances/
```

Confirm it with `git check-ignore local-instances/<instance>/telemetry.db`.
This is defense in depth against accidental commits, not a substitute for
keeping the instance outside target repositories.

## Choose one of three config-source placements

With instance-local config, `instance.yaml` and `config/` are the canonical
files. A checked-in source instead contains `instance.yaml.example`,
`manifest.yaml`, and `gaggles/`. That source is validated and materialized into
the instance's active config; author the source, not the materialized copy.

| Placement | Choose it when | Review and permission boundary |
|---|---|---|
| **`config/` inside the instance root** | One operator runs a private instance and does not need repository review of workforce changes. | The host account is the boundary. Changes are local and are not protected by CODEOWNERS or branch rules. |
| **Non-empty subtree in the target repo** | You own the target, may add config files and CODEOWNERS, and want workforce changes versioned with project changes. | Set the config root to the subtree, such as `reference-workflows`; own that path with CODEOWNERS and require its review through branch protection. Path confinement limits proposed diffs, but a repository credential is not path-scoped. |
| **Separate config repository** | The target is third-party, you cannot change its governance, several people operate the instance, or you want the strongest boundary. | Scope the config credential only to the config repository and protect that repository. It cannot write project code. Keep the target credential separate and grant only the workflow operations the target needs. |

### Instance-local `config/`

This is the simplest layout:

```text
~/goobers/instances/private-widget/
  instance.yaml
  config/
    manifest.yaml
    gaggles/
  gaggles/
  scheduler/
  telemetry.db
```

Use it for a single-operator, private experiment where filesystem ownership and
backups are sufficient governance. It is a poor fit for a Tutor or any workflow
that should propose config changes through pull requests. Once changes need
normal repository review, move the canonical definitions to one of the two
versioned layouts below and materialize them into the instance.

### In-repo subtree ("same repo")

This layout versions config beside project or platform code:

```text
~/src/project/
  .github/CODEOWNERS
  src/
  reference-workflows/                       # canonical Goobers config source
    instance.yaml.example
    manifest.yaml
    gaggles/

~/goobers/instances/project/      # runtime state, outside ~/src/project
```

The configured config root **must be a non-empty repository-relative subtree**,
never the whole target repository. Add a CODEOWNERS rule for that root, such as
`/reference-workflows/`, and require CODEOWNER review with branch protection. These controls
make "agents propose; repository governance decides" load-bearing: an agent or
Tutor may open a config pull request, but it cannot make that proposal active by
editing the instance or merging around review.

The subtree boundary also has a consequence: GitHub credentials are scoped to a
repository, not to `/reference-workflows/`. Goobers fails closed when a confined config
proposal touches another path, but the credential itself can reach project code
if it has repository contents access. Use the [Tutor config-only
write-boundary](tutor-write-boundary.md) and its CODEOWNERS and branch-protection
checklist. Choose a separate config repository instead when path confinement is
not strong enough for the deployment's trust posture.

### Separate config repository ("outside")

This is the recommended layout for third-party targets and team operation:

```text
~/src/widget-goobers-config/      # canonical, dedicated config repository
  instance.yaml.example
  manifest.yaml
  gaggles/

~/goobers/instances/widget/       # runtime state
  instance.yaml
  config/                         # materialized from the source above
  gaggles/
  scheduler/
  telemetry.db
```

Protect the config repository with required validation and review. Scope every
config-source credential, both read access for delivery and write access for
proposed changes, only to that repository. Give the target provider credential
access only to the target repository and only for declared workflow operations.
The whole config repository is the config root, so no platform or target code
is present for a config-writing credential to modify.

This layout does not prevent target-side agent instructions. Files such as
`.github/copilot-instructions.md` or `.claude/` may remain in the target
repository and arrive naturally in each managed workcopy. They guide an agent
working on that project; Goobers workflows, capabilities, gates, and credentials
remain governed by the separate config source.

## Decision table

| Situation | Recommended placement | Reason |
|---|---|---|
| You are the only operator of one private target and do not need config pull requests. | Instance-local `config/` | Lowest setup overhead; the local account is the accepted trust boundary. |
| You own one target and can add a config subtree, CODEOWNERS, required checks, and branch protection. | In-repo subtree | Project and workforce changes share history while the config root remains explicit. |
| You do not own the target or cannot add files and enforce CODEOWNER review. | Separate config repository | Goobers adds nothing to the target and config governance remains under your control. |
| You own the target but require a credential-level boundary between autonomous config changes and project code. | Separate config repository | A credential scoped only to the config repository cannot reach project code. |
| Several operators maintain the workforce. | Separate config repository | Normal repository access, review, and audit rules protect desired state. |
| You operate several target repositories together under one operator and credential posture. | One outside instance root with one gaggle per repository | Repository selection is gaggle-aware: each gaggle's `project` and `backlog` connections select their own `repos` entry. A few built-in behaviors still bind to the first `repos` entry — see the [single-repo residue](arbitrary-repo-onboarding.md#current-single-repo-residue) note. |
| You need an isolation boundary between target repositories — different credentials, trust postures, or machines. | One outside instance root and config source per target | Separate roots isolate credentials, journals, budgets, and workcopies. |

Several gaggles may share one configured target, and separate gaggles may
target different `repos` entries within the same instance. Both need unique
workflow identities and disjoint backlog routing, but neither requires another
placement model. Create another outside instance root when a repository needs
its own isolation boundary, not because of the repository count.

## Credential and trust checklist

Placement complements the capability model; it does not replace it:

1. Keep secret values out of `instance.yaml`, `instance.yaml.example`, and
   workflow YAML. Reference environment variables, protected files, or a
   configured secret store.
2. Separate model credentials, target provider credentials, and config-source
   credentials. Do not give a config credential target-repository access merely
   because both belong to the same operator.
3. Grant target capabilities per goober and workflow. A separate config
   repository does not reduce the effects of a stage deliberately granted
   target write access.
4. Require validation and review before config changes merge. Agents and the
   Tutor propose changes; repository governance decides which definitions
   become desired state.
5. Treat target backlog text and repository content as untrusted inputs. Keep
   trust labels, independent review gates, and branch protection in place
   regardless of filesystem layout.

## Worked example: external `JeffSteinbok/hass-dreo`

`JeffSteinbok/hass-dreo` is a third-party Home Assistant Python integration.
The operator does not want Goobers config committed to it, so use three distinct
locations:

```text
~/src/hass-dreo-goobers-config/   # private or reviewed config repository
~/goobers/instances/hass-dreo/    # instance root and runtime state
~/src/hass-dreo/                  # optional human checkout, not used as runtime state
```

Set the config source's target connection to `JeffSteinbok/hass-dreo`, then
initialize the outside instance using that source:

```sh
export GOOBERS_INSTANCE="$HOME/goobers/instances/hass-dreo"
export GOOBERS_CONFIG_SOURCE="$HOME/src/hass-dreo-goobers-config"
export GOOBERS_TARGET="JeffSteinbok/hass-dreo"

goobers init --guided
goobers validate --source-tree "$GOOBERS_CONFIG_SOURCE"
goobers config materialize "$GOOBERS_INSTANCE"
goobers validate "$GOOBERS_INSTANCE"
```

During the tutorial, select `$GOOBERS_CONFIG_SOURCE` as a custom configuration
folder and provide the existing local clone of `$GOOBERS_TARGET`. The managed
target checkout then lives
under `$GOOBERS_INSTANCE/gaggles/<gaggle>/workcopies/`. The target's existing
`.github/copilot-instructions.md` and `.claude/` guidance stays in `hass-dreo`
and is available in those workcopies; instance config and runtime state stay
outside it.

Continue with [Onboard an arbitrary repository](arbitrary-repo-onboarding.md)
for least-privilege tokens, generated definitions, trust labels, and the first
curation-to-PR cycle.

## Worked example: Goobers dogfood under `reference-workflows/`

The Goobers project owns its target repository and can enforce governance on a
dedicated subtree, so its dogfood config uses the same-repo model:

```text
~/src/Goobers/
  .github/CODEOWNERS              # owns /reference-workflows/
  cmd/
  internal/
  reference-workflows/                       # canonical config source

~/goobers/instances/goobers/      # runtime state, outside the checkout
```

Here the config root is `reference-workflows`, not an empty root and not the repository
root. Tutor proposals are confined to their declared action root, `/reference-workflows/`
has a CODEOWNER, and branch protection governs whether a proposal merges. The
instance still creates its managed `Agent-Clubhouse/Goobers` copy and run
worktrees beneath the outside instance root. See the
[self-hosting runbook](../../reference-workflows/README.md) for the repository-specific
setup and guardrails.
