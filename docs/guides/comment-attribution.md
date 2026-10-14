# Goobers comment attribution

Every issue comment, pull-request comment or review, and issue body written by a
workflow stage ends with a versioned attribution marker and a visible summary:

```text
<!-- goobers:attribution v1 <base64-json> -->
Posted by **Goobers** | `gaggle/workflow` | task `task` | goober `role` | run `12345678` | version `v1.2.3`
```

The Base64 value decodes to UTF-8 JSON with this schema:

```json
{
  "schema": 1,
  "goobers": true,
  "instance": "MDB1",
  "gaggle": "dogfood",
  "workflow": "implementation",
  "task": "escalate",
  "goober": "implementer",
  "run": "224712dcde5c4deda9717a03a8c26770",
  "action": "comment"
}
```

The visible footer reports the Goobers binary version that wrote the comment.
The version is not duplicated in the hidden attribution JSON.

Consumers should locate the `goobers:attribution v1` marker, require exactly one
marker, Base64-decode its payload, and branch on both the marker version and JSON
`schema` before interpreting fields. The `goobers` flag is always `true`. Go
consumers can use `providers.ParseAttribution`.

Before appending its trusted marker, the provider removes any pre-existing
attribution marker from generated content. Base64 encoding prevents
run-controlled names from terminating the HTML comment. Once any run context is
present, writes fail closed when required fields are missing, contain control
characters, or exceed 256 characters; a partially populated run can therefore
never silently publish an unattributed comment.

Claim and claim-release comments retain their existing
`goobers-claim: run=...` markers for compatibility and also carry this general
attribution block.

### Claim-release reasons

A claim-release comment states why the claim ended (#4421). Its first line is
still the release boundary, `goobers-claim-release: run=<id>`, unchanged, so
claim-epoch detection and historical comments keep parsing. A versioned
machine-readable reason line follows in its own paragraph:

```text
goobers-claim-release-reason: v=1 disposition=<disposition> [stage=<stage>] [code=<code>] [actor=<actor>]
```

- **Fields.** The fields are space-separated `key=value` pairs. Values use a
  restricted alphabet (ASCII letters, digits and `._-:+`, at most 64 bytes),
  so a reader splits on whitespace and on the first `=` without unescaping.
- **Dispositions.** `disposition` is one of `completed`, `failed`,
  `escalated`, `cancelled`, `abandoned`, `no-work`, `stall-swept`,
  `reconciled`, `force-released`, `superseded` or `unknown`. `stage`, `code`
  and `actor` appear only when known.
- **Reading it.** Ignore unknown keys. Treat a missing line, or any version
  other than `v=1`, as an unknown (legacy) release.

The human-readable `Reason:` line and an eligibility statement follow the
reason line. The summary in the `Reason:` line is bounded to 240 bytes. It is
scrubbed of secret shapes, credential assignments and local paths. HTML
delimiters and `@` mentions in it are neutralized. The machine line never
carries the summary. The same structured reason is recorded on the claim
ledger's history entry and on the `claim.released` / `claim.force_released`
instance-journal event (`claimRelease`).

## Reading back text Goobers wrote

The forge stores the body with the attribution block appended, and provider
calls return that stored body: the result of a create, update or reply, and
every later list or get. Code that reads back text Goobers wrote and compares
it with the text it meant to write must remove the block first. In Go, compare
`providers.StripAttribution(stored)` with `strings.TrimSpace(intended)`. Never
compare the raw body exactly, by suffix, or by token similarity.

The `cmd/goobers` test suite runs stage providers with attribution on by
default, as the daemon does, and the fake forges store Goobers' own seeded
comments with the block. A test that pins the unattributed standalone behavior
opts out with `withoutDefaultStageAttribution(t)`.
