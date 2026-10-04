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
