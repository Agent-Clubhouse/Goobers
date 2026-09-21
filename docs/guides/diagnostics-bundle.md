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

## Independent diagnostic export

Local instance health evidence is retained whether or not export is enabled.
To send operational observations to your company's OTLP/gRPC **Logs** collector,
configure its destination explicitly in `instance.yaml`:

```yaml
telemetry:
  diagnostics:
    otlp:
      endpoint: https://collector.example.com:4317
      headers:
        authorization:
          env: COMPANY_DIAGNOSTICS_AUTH
      tls:
        caFile: /etc/company/collector-ca.pem
```

The diagnostic destination and credentials are independent of the run journal
collector in `telemetry.otlp`. Both destinations may be the same. Neither
`GOOBERS_OTLP_*` nor `OTEL_EXPORTER_OTLP_*` variables opt an instance into
diagnostic export. Set `enabled: false` inside either `otlp` block to disable
that stream explicitly; for journal export this overrides ambient defaults.
`telemetry.enabled: false` disables run telemetry, while diagnostic export
remains independently configurable. No destination means no diagnostic client,
DNS lookup, or connection. Configuring a company collector sends nothing
upstream to Goobers maintainers.

The initial record is `goobers.service.health`, emitted at daemon startup and
every six hours. Its resource identifies the Goobers version, build commit,
and `goobers.telemetry.stream=diagnostics`. The record includes durable instance
identity when available, machine/account names, process uptime, observed dirty
restarts and their history coverage, and recovery inventory occupancy. Account
name is runtime identity, **not an owner or outreach address**. It excludes
inventory paths, arbitrary journal payloads, raw errors, prompts, and code;
exported strings also pass through registered-secret and pattern scrubbing.
Unknown history coverage does not emit a zero restart count.

This cadence is historical health evidence; it is not a live fleet heartbeat
or proof that a deployment is healthy between observations. The separate `goobers.fleet.heartbeat` record supplies a faster observation
cadence and explicit company/owner metadata. Feature usage and approved-version
assessment are separate parts of the diagnostic rollout.

Export is best effort: each request has a two-second deadline, records are
limited to 64 KiB. Fleet records are batched into at most 128 records and
1 MiB per request; queued requests are capped at 128 and 8 MiB of encoded
payload, plus one in-flight request. Each pulse emits at most 101 health records
and 96 feature records, sampling feature use for eight gaggles per pulse in a
rotation. Requests batch these records subject to both record and byte limits. Full queues, rejected
records, transport failures, and shutdown losses are counted. Clean daemon
shutdown writes a local `diagnostics-export-summary` annotation with accepted,
delivered, dropped, and failed counts. A collector outage does not block
workflow execution or local journaling. Collection and sharing of the support
bundle above remain explicit operator actions.

## Fleet observations and offline evidence

```yaml
telemetry:
  diagnostics:
    organization: example-company
    environment: production
    ownerRef: team:operations
    gaggleOwners:
      application: team:application
    heartbeatInterval: 30s
    progressTimeout: 30m
```

These labels identify operator-provided routing references; they never infer
ownership from machine or account names. Collector authentication determines
company access. The existing durable root identity identifies the deployment
and instance; gaggle names are scoped by that identity. Each daemon lifetime
has a random boot identifier and monotonically increasing observation sequence.

Fleet observations begin during daemon startup and remain in the private
`scheduler/diagnostics/history.json` snapshot even without an exporter. The cadence is configurable between ten
seconds and one hour. Eligible-work stall thresholds range from one minute to
seven days. Startup, intentional pause, retry backoff, and observation gaps do
not accrue time toward a stall. Successful completed work advances useful
progress; repeated no-work completions do not.

The sampler uses bounded indexed read-model queries. An unavailable index,
stale or incomplete eligibility evidence, or unknown claim availability yields
unknown, rather than an exhaustive scan or a healthy zero. Selection eligibility
alone does not establish that work is available to claim. The current queue
observation comes from recorded PR selection evidence; workflows without that
evidence remain unknown. Each pulse is limited to 100 gaggles and 100 runs or
workflows per gaggle, with a five-second read deadline. Truncation is partial
coverage. Local history retains at most 4 MiB and 4,096 records, with one bounded
scratch file during replacement. Evictions, omissions, known write failures and
resets are reported explicitly. The six-hour service-health record remains in
the instance journal. See the [fleet diagnostics reference](fleet-diagnostics-reference.md)
for sampling, retention and evidence limits.

The offline support bundle includes a projected `operational` section with
build/platform, instance/gaggle/boot identifiers, observation window, last useful
progress, state and coded reason. It reads the bounded diagnostic snapshot,
retaining at most 100 identities and reporting missing or truncated evidence.
If the snapshot is unavailable, it falls back to the latest 4 MiB and 1,000
legacy instance events. It excludes routing contacts, prompts, code and raw errors.
The six-hour historical health observation uses that bounded legacy tail and never
reports zero restarts when earlier history was omitted. Preview the generated
JSON/summary before deliberately sharing it upstream; collection sends nothing.

## Related

- [GitHub token scopes](github-token-scopes.md)
- [Backlog routing diagnostics](backlog-routing-diagnostics.md)

### Engine worker observations

Fleet heartbeats consult the accepted scheduler definition snapshot. Local-only
workflows do not trigger a Temporal query, even when engine configuration exists.
For admitted engine workflows, the daemon reuses its existing Temporal client to
inspect the shared engine queue's workflow and activity pollers once per pulse.
Both queries together have a one-second limit (500 ms per RPC); definition and
poller inventories are capped at 1,000 entries. This runs in the health observer,
without delaying model execution or scheduler admission.

`workerObservation` is `recent_poller` when both task types have a poller whose
last access is within two minutes; `no_recent_poller` means a query found an empty
inventory or only older pollers. A five-second clock tolerance permits poller access to advance during the RPC;
farther-future timestamps remain unknown. Missing/invalid timestamps, unavailable
queries, or incomplete admission inventory produce `unknown`, without retaining
a previous missing-worker claim. A local-only gaggle reports `not_required`.
`missingWorkerCount` is 0 or 1 for the observed shared queue requirement; it is
omitted when unknown or not required. A known missing requirement yields
`worker_unavailable`, not a claim that a process crashed.

`workerCoverage=engine_workflow_activity_queue` makes this limit explicit:
recent polling does not prove a dispatch worker or pod can execute a particular
stage. Configured but unused dispatch queues are not treated as missing workers.
Mixed gaggles observe this engine dependency only for their admitted engine
workflows; local workflows continue independently. Poller identities, queue names,
frontend addresses and raw RPC errors are not exported.
