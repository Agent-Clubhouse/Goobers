# Instance-shared goobers

Define a persona once under `goobers/<name>/`, beside the `config/` directory,
to make it available to every gaggle included in the instance manifest:

```text
repository/
  config/
    manifest.yaml
    gaggles/team-a/...
    gaggles/team-b/...
  goobers/reviewer/
    goober.yaml
    instructions.md
  skills/review/SKILL.md
```

The shared `goober.yaml` omits `spec.gaggle`:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Goober
metadata:
  name: reviewer
spec:
  role: reviewer
  instructions: instructions.md
```

Either gaggle can reference `goober: reviewer` in an agentic task or gate.
There is one persona identity, not one copy per gaggle. The optional
`spec.workflows` list resolves workflow names across gaggles; it does not
assign ownership. Shared definitions are checked against all configured DSL
pins.

Directory placement defines the scope. Shared definitions must live directly
under their matching `goobers/<name>/` directory and must not declare a gaggle.
Gaggle-local definitions continue to require `spec.gaggle`; referencing another
gaggle's local persona remains an error. The shared tree accepts Goober
definitions only, and its root cannot be a symlink.

Names remain unique across the whole instance. A local and shared persona with
the same name is a validation error, not an override. To migrate a persona,
move its directory to the shared tree and remove `spec.gaggle`; do not leave
the old definition behind.

Instructions and assets resolve relative to the shared persona directory.
Shared personas' declared skill content is resolved from the instance-level
`skills/` tree for identity and snapshotting. A gaggle's same-named skill
override does not change a shared persona; local personas keep their existing
gaggle-first skill resolution.

Keep the sibling trees together when publishing a config source or seeding a
worker. Both the local loader and config-sync include shared definitions,
including when config-sync stages a source to exclude its render output.
The definition watcher includes shared instructions and referenced skill
content. New admissions use the updated definitions; retained worker snapshots
continue to serve an in-flight run's pinned persona. A pin whose snapshot is
no longer retained is refused, not substituted with current disk content.

Validate a running instance's source using `goobers validate <instance-root>`,
or a checked-in source tree using `goobers validate --source-tree <repo-root>`.
Validating an isolated shared-persona directory does not
provide the manifest and workflow context needed for reference checks.

## Skill package format

A `spec.skills` entry is a bare name, resolved to a directory: a gaggle-local
persona looks under `config/gaggles/<gaggle>/skills/<name>/` first, falling
back to the instance-shared `skills/<name>/` beside `config/`; a shared
persona (no `spec.gaggle`) only ever resolves the shared path. The name
itself may not contain a path separator or resolve outside `skills/`.

Whichever directory is found, every regular file under it (recursed, in
sorted path order) is loaded as the skill's content — there is no required
filename or manifest. The convention this repo's own shipped skills follow
(for example `skills/goobers-dsl-author/` in the repo root, unrelated to a
goober's own declared skills but the same shape) is a `SKILL.md` entrypoint
plus an optional `references/` subdirectory of supporting files, matching
what the harnesses' own skill-invocation format expects — but the loader
does not parse or require `SKILL.md` specifically; any files under the
directory are included as-is.

`goobers validate`'s `SKILL002` check is existence-only: it confirms the
resolved directory exists and is a directory, not that it contains
`SKILL.md` or any other content. An empty directory created with `mkdir`
satisfies the check exactly as well as a real skill package.
