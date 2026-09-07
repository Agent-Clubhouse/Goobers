# Diagnostics bundle

`goobers diagnostics bundle` collects a portable, redacted support bundle from
an installed binary and an instance directory alone.

The problem it solves is a support prerequisite, not a missing feature. During
the 2026-08-15 dogfood incident an operator holding a released binary, an
instance root and its run journals still had to open a separate checkout of the
Goobers source and grep implementation files — `prselect.go`, credential
resolution, remediation checkpoint logic — to answer basic production
questions. Reading Goobers' own source should never be part of reading a
Goobers incident. Issue
[#2968](https://github.com/Agent-Clubhouse/Goobers/issues/2968).

```
goobers diagnostics bundle ./my-instance
goobers diagnostics bundle --run 8f2c --output /tmp/incident.tar.gz ./my-instance
goobers diagnostics bundle --pr 4123 --json ./my-instance
```

## What it contains

| Section | Answers |
| --- | --- |
| `binary` | Which build made these decisions — version, commit, date, OS/arch, Go. |
| `contract` | The journal schema this binary writes, the DSL versions it admits, and **every built-in stage command with the capabilities it requires**. This is the half that previously meant reading `internal/providerstage/manifest.go`. |
| `instance` | The loaded config generation: a digest over the definitions, plus each gaggle's workflows (name, DSL version, definition digest) and goobers. |
| `daemon` | Running, not running, or — the case that reads as healthy and is not — holding the lock but not ticking. |
| `credentials` | Every declared credential by **capability, source kind and source name**, and whether its source is present. |
| `runs` | Per run: identity and the workflow digest it was pinned to, phase, window, the stage timeline with artifact **metadata**, the selector's own inclusion/exclusion reasons, and the decisive error in full rather than truncated. |
| `notes` | What the collector could **not** read, and why. An omission stated is not an omission found. |

The archive holds two files: `diagnostics.json` (machine-readable) and
`summary.md` (human-readable, ordered by what an operator actually asks).
`--json` writes the document to stdout instead.

## Why it cannot contain a credential

Two mechanisms, and the first is the load-bearing one.

**Structural.** The bundle projects only fields that cannot carry a secret. A
credential contributes its capability, its source kind and its source name; the
collector never resolves one, and there is no field on the record that could
hold a value. Presence is answered the cheap way — an environment variable is
set, a file exists — and for a keychain, secret store, or GitHub CLI source the
bundle reports *"presence not determinable without resolving it"* rather than
guessing, because asking the resolver would materialize the secret. Agent
transcripts and stage stdout are excluded **wholesale rather than filtered**:
they are where a prompt can quote a token, and a bundle that summarizes them
cannot leak what it never reads. Artifacts contribute metadata, not content.

**Textual.** Every string that survives that projection then passes through the
same secret-pattern net the journal writes behind, so a token pasted into an
error message is redacted on the way out too.

The config generation is reported as a **definition** digest, not the compiled
`Machine.Digest()` a run journal pins. Computing the latter means compiling with
the daemon's full option set, which resolves the `agent:model` credential — and
materializing a secret is exactly what this bundle must never do. A run's own
compiled digest is reported straight from its journal, where it was already
recorded, so both facts are available without either being mislabelled.

## Reproducibility

Two collections of the same instance state produce **byte-identical** archives
apart from the collection timestamp. Every tar header is fixed (same mode, zero
uid/gid, no uname/gname, the bundle's own `generatedAt` as the modtime), the
gzip header carries no name or timestamp, and every list is sorted by a stable
key. That is what makes "reproduce it on the support machine" a testable claim
rather than an aspiration, and it means two bundles can be diffed to show only
what actually changed.

## Explaining a no-work cycle

This is the case the bundle was built for. A selector that finds nothing now
records **why** as a scalar stage output (`noWorkReason`), so the account lands
in the run journal rather than only in the stage's stdout — which a redacted
bundle cannot carry. `pr-select` threads its full exclusion tally into that
reason, so a bundle shows

```
pr-select excluded (noWorkReason): queue parked: 7 of 7 matching pull
request(s) excluded — escalated, human action required 7
```

instead of a generic "no eligible PR to select this cycle". That is the
distinction between *nothing to do* and *everything is parked*, which a healthy
daemon reports identically without it (#2969).

## Related

- [GitHub token scopes](github-token-scopes.md)
- [Backlog routing diagnostics](backlog-routing-diagnostics.md)
