# Tracked gaggle templates

Tracked templates let you **copy a gaggle and keep its ancestry**. You own
ordinary editable YAML and Markdown in your configuration repository. Goobers
remembers the original template, checks for upstream changes, and merges an
update only when you explicitly request it.

This is an opt-in configuration-management feature, not runtime inheritance.
Existing gaggles without tracking metadata keep their current behavior.
It does not change the workflow DSL, automatically apply templates, push to
either repository, or send email.

## The three locations

1. **Template repository:** the author's reusable, committed gaggle package.
2. **Your configuration checkout:** your complete customized definitions and
   tracking metadata. This is the repository you review and commit.
3. **Runtime instance:** the deployed copy. Backprop captures edits made here
   into your configuration checkout, **not into the template repository**.

Use a separate configuration source for update and backprop. Imports can also
target an instance's own `config` directory for standalone evaluation, but
that is not a substitute for a durable config repository.

## Build a template package

The repository includes this minimal package at
[`templates/starter`](../../templates/starter). It has one manual inspection
workflow and no automatic triggers. Copy it into your template repository or
import that directory from a branch containing this feature.

A template is a **self-contained directory containing exactly one gaggle**:

```text
template-repo\
  templates\
    starter\
      gaggle.yaml
      workflows\
        inspect.yaml
      goobers\
        reviewer\
          goober.yaml
          instructions.md
          assets\
            review-checklist.txt
      skills\
        review\
          SKILL.md
```

The `goobers`, `skills`, and `assets` directories are optional. If you reference
them, include their contents in the package. Do not depend on an adopter
already having a shared persona or sibling skill installed. Missing skill
packages, missing instructions, or invalid references are rejected, including
missing-skill conditions that ordinary config validation treats as warnings.

For a minimal package, create `gaggle.yaml`:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: starter
spec:
  displayName: Starter
  project:
    provider: github
    owner: your-org
    name: your-repo
    branch: main
  backlog:
    provider: github
    project: your-org/your-repo
  isolation:
    namespace: gaggle-starter
```

And `workflows\inspect.yaml`:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: "2.0"
metadata:
  name: inspect
spec:
  gaggle: starter
  triggers:
    - type: manual
  start: inspect
  tasks:
    - name: inspect
      type: deterministic
      goal: Inspect the configured project working copy.
      run:
        command: ["git", "status", "--short"]
```

For agentic workflows, use the ordinary
[workflow primitive reference](../reference/workflow-primitives/README.md)
and [DSL authoring guide](dsl-authoring-skill.md). Each persona must belong to the
template gaggle and use package-local instructions and skills. The importer
prefixes persona names with the destination gaggle name and rewrites typed
task/gate references, allowing the package to be imported under different names.
It does **not** rewrite names embedded in prose, shell commands, or arbitrary
task input strings. Keep those portable or document the required local edits.

Commit the package to a branch before importing: uncommitted template edits are
not visible. A template does not contain `instance.yaml`, a deployment manifest,
credentials, installation hooks, or pre-generated `.template` metadata.

Initial package constraints:

- One YAML document per definition file; expand YAML aliases before importing.
- Branch tracking only, with an exact full commit recorded in the generated
  lockfile. Tags and arbitrary commit arguments are not supported.
- Regular files only; no symlinks or automatic external dependency fetching.
- At most 2,048 files and 16 MiB of package content.
- Shared dependencies must be vendored into the gaggle package. Updates to
  vendored skills/assets are then tracked along with the YAML.
- Declared DSL versions must be supported by the installed Goobers binary;
  preview features remain subject to ordinary configuration policy.

## Import into your configuration source

Create an instance and a separate config source using the
[quickstart](quickstart.md) or guided setup. The source must already have a valid
`manifest.yaml` and a `gaggles` tree; use its own `instance.yaml.example` for
local-source materialization.

The following PowerShell example uses local Git checkouts:

```powershell
goobers down C:\Goobers\orders
goobers config templates import `
  --repository C:\src\template-repo `
  --directory templates\starter `
  --ref main `
  --gaggle orders `
  --source C:\src\orders-config `
  C:\Goobers\orders
```

`--source` defaults to the instance's local `workflowSource.path`, or its own
`config` directory when no workflow source exists. A remote workflow source
requires an explicit writable local checkout via `--source`.

For an HTTPS template repository, use `--repository` with its HTTPS clone URL
and `--token-env GOOBERS_TEMPLATE_TOKEN`. Supply that variable through your
normal secret-management/service environment. It must be read-only for the
template repository; no write access is required. Public HTTPS sources use the
same explicit-token contract as existing Git workflow sources. A local clone
is the alternative when you prefer to authenticate/fetch outside Goobers.
Do not place a token in the URL or source YAML.

The import adds the gaggle to **your source manifest**, creates the normal
files, and generates tracking metadata. Existing destinations are refused.
The imported gaggle starts with `spec.enabled: false`: it must not begin work
against the template author's repository.

Before enabling it:

- Set its project, backlog, CI command, budgets, identity and other local policy.
- Ensure your instance's repo/credential configuration supports that project.
- Review workflow actions and permissions as executable configuration.
- Set `spec.enabled: true` only after review.
- Validate, review and commit your config repository.

Deploy through your existing source workflow. For a local source recorded in
`instance.yaml`:

```powershell
goobers validate --source-tree C:\src\orders-config
goobers config materialize C:\Goobers\orders
goobers up C:\Goobers\orders
```

For Git-backed deployment, commit/review/merge to the instance's configured
branch and use its existing reconciliation path. The template commands do not
change that branch or bypass repository review.

## What is the new YAML?

The importer generates `gaggles\orders\.template\source.yaml`:

```yaml
schemaVersion: 1
repository: C:\src\template-repo
ref: main
directory: templates/starter
gaggle: orders
```

`directory` is normalized to a portable repository-relative path. HTTPS
sources additionally record `tokenEnv`, an environment-variable **name**, not
its value.

The same hidden directory holds:

| File | Ownership and purpose |
| --- | --- |
| `source.yaml` | Generated enrollment settings; commit with your config. |
| `lock.json` | Accepted full commit, source identity, content digest and pristine bound file contents; commit with your config. |
| `deployed.json` | Runtime-only snapshot of the last deployed **user configuration**; generated during materialization. Do not backprop or commit it. |

The source-edit lock is a persistent local lock file, not configuration.
Keep operational files out of your config commits, for example with:

```gitignore
.template-edit.lock
.template-config.lock
**/.template/deployed.json
**/.template-candidate-*/
**/.template-backup-*/
```

Do not ignore the whole `.template` directory: `source.yaml` and `lock.json`
must travel with the customized gaggle.

The two baselines are deliberately different. `lock.json` answers "what came
from the template?" `deployed.json` answers "what changed at runtime since
deployment?" Never edit a lockfile to accept an update. Unknown source fields,
source/lock disagreement and corrupt baselines fail closed.

The template loader ignores upstream `.template` metadata rather than trusting
it. Existing definition discovery skips the hidden directory; runtime YAML
remains ordinary Gaggle, Goober and Workflow documents.

## Customize, then backprop runtime edits

Prefer editing your own source files and deploying them. All normal schema-valid
customizations remain available: schedules, budgets, instructions, workflow
structure, added files and deletions. The template is not an allowlist of knobs.

If you edit the runtime copy, including through a workflow toggle:

```powershell
goobers down C:\Goobers\orders
goobers config templates backprop `
  --gaggle orders `
  --source C:\src\orders-config `
  C:\Goobers\orders
```

Goobers compares the last deployed user-source snapshot, runtime edits, and
the current checkout. Independent edits merge; competing edits stop without
changing the checkout. A successful capture changes only that gaggle's files
and retains the checkout's template tracking. Review and commit those files in
**your repository**, then deploy. No commit, push, PR or upstream write is
performed automatically.

Backprop requires the source gaggle to already have the same template source
identity. It is not a generic "copy this runtime to any repo" command or an
ancestry-guessing migration for old manually copied gaggles.

Until deployment, runtime still differs from its last deployed snapshot; the
pending indicator can therefore remain after capture. Deployment clears it by
recording a new user-source baseline. Repeating capture is safe.

Both Git-source replacement and local materialization refuse to overwrite
tracked runtime edits that are absent from the candidate source. Legacy,
unenrolled gaggles retain their previous replacement behavior.

## Learn about updates

The daemon checks enrolled runtime gaggles at startup and every 24 hours.
Checks fetch only the template source and never apply an update. With a local
template checkout, fetch/update its local tracked branch yourself first;
Goobers observes that committed branch rather than fetching its remote.

```powershell
goobers config templates check C:\Goobers\orders
goobers config templates status C:\Goobers\orders
goobers status C:\Goobers\orders
```

The portal gaggle page and its existing API inventory show cached state,
installed/candidate revisions, changed files, conflicts, last successful check
and pending runtime edits. Daemon notices are deduplicated by package digest
and state, including across restarts. Commits outside the package do not create
false update alerts. Source checks use a two-minute per-sweep timeout.

API reads and status commands never fetch. An offline or failed check is
`unknown`, not "up to date"; an old successful check becomes `stale`.
After deploying a new lock, cached status becomes unknown until checked again.
Rewritten history, inaccessible old commits and removed packages require
provenance review instead of silently accepting a different source.

## Review and apply an update

Capture any runtime edits first, then:

```powershell
goobers down C:\Goobers\orders
goobers config templates update `
  --gaggle orders `
  --source C:\src\orders-config `
  C:\Goobers\orders
git -C C:\src\orders-config diff
```

The update compares **old template / customized source / new template**.
Upstream-only changes are taken, local-only changes stay, identical changes
agree, and overlapping changes conflict. A deleted upstream file with local
edits is a conflict, not permission to delete it.

YAML maps merge by key. Workflow `spec.tasks` and `spec.gates` merge by unique
`name`; other lists are atomic to avoid inventing ordering or trigger identity.
Reordering tasks/gates while the other side edits that list is a conflict.
Markdown, other assets, and competing permission changes are conservative
whole-file conflicts. This version does not do line-level text merging.
Untouched files are preserved byte-for-byte; merged YAML may be reindented.

On success, the new pristine template becomes the next baseline, not the
customized merged result. Review and commit both normal files and the lock,
then deploy. Updates never directly modify a running instance.

On conflict, accepted files and lock remain unchanged and the command lists
conflicting files/fields. Resolve in a review branch of **your** repo:

1. Preserve your current local customization in Git or a separate backup.
2. Inspect the named upstream files at the reported candidate revision. For
   the conflicting fields only, choose the upstream value temporarily and
   retry the update. Do not discard unrelated local changes.
3. Reapply any deliberately retained local customization against the now
   accepted template, validate and review the final diff, then commit.

There is no blind `--force`/ours-wins switch. Source-origin changes, branch
rewrites, and manual lock editing are not a supported conflict-resolution path.

## Failure recovery and release boundaries

Mutating template commands hold the instance's existing daemon lock and a
source-edit lock. Stop the instance first; do not hand-edit the same checkout
concurrently. Portal workflow edits and managed Git-source replacement share a
separate config lock. Candidate validation runs against the complete target
configuration before publication; missing/invalid dependencies never become
accepted templates.
The lock also covers a deployment that enrolls an instance's first tracked gaggle.

A gaggle publication uses a sibling candidate directory and a hidden
`.template-backup-<gaggle>` directory. If interrupted, retain both versions and
inspect them while stopped. Restore the selected complete gaggle **including
its tracking metadata** from Git or the backup, validate the source, and only
then remove the named recovery directory and retry. Publication refuses to
overwrite a retained recovery backup. Do not delete an entire instance to
recover a template operation.

Import also changes `manifest.yaml`. If registration fails after the disabled
gaggle was written, the command explicitly reports the partial operation;
register the completed gaggle or restore both source changes from your review
branch before deployment. This is not an atomic transaction with Git, and
successful local capture does not mean a commit was pushed or merged.

For release selection: this initial version supports **self-contained,
branch-backed packages** and explicit offline mutations. It does not include
automatic adoption of old copies, cross-package dependency resolution,
line-level prose merging, tag selection, source retargeting, email delivery,
or automatic repository commits. Validate the source/authentication and OS
combinations you plan to support before assigning a release.

Older binaries can read the ordinary definitions when their schema/DSL is
compatible, but they do **not** enforce tracked-template overwrite protection.
Before downgrading, stop the daemon, backprop/commit pending edits and retain a
complete config snapshot; do not run two binary versions against one instance.
