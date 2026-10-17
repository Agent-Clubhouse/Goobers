# Interactive DSL 3.0 sample

This is a normal configuration directory: Manifest, Gaggle, existing Goobers with
Markdown instructions, and a Workflow. Validate or load it through the same
configuration path as ordinary workflows. It contains no credentials, connections
or grants. The ADO tuple and both broker references designate controlled local
fixtures in the experimental worker; they do not establish live ADO/Azure access.

`Gaggle.dslVersion: "3.0"` pins only `spec.interactive`. Each Workflow retains its
own version and its own preview acknowledgement. The Gaggle acknowledges the
interactive preview separately. Legacy Gaggles without this block are unchanged;
`interactive: {}` exposes no surfaces. Interactive children are a reviewed worker
adapter, distinct from DSL 3.1 `task.childWorkflows`.

Both surfaces default to `codex-local`. The host must independently enable trusted
local model access; otherwise the requested mode fails explicitly. Select
`local-tools` explicitly for the deterministic local fixture demonstration.
Goober `model` is omitted to use the existing Codex adapter default; operators may
set an approved model before launch. No network or model call occurs during
configuration validation.

Surface `tools` allow only named mediated host MCP operations. They are distinct
from `Goober.spec.tools` and do not constrain Codex's built-in filesystem/shell
capabilities: the experimental native adapter trusts the local harness and its
workspace permissions. This sample adds no external MCP servers or credential
references. Omitted tool, workflow and authority lists grant none. Authorities are
opaque host broker lookups; the broker alone resolves identity and operation scope.

Session values are ceilings which a worker can narrow. Local coordination is a
single exclusive controller with durable leases, not distributed multi-writer
storage. Children require explicit human review and an idle parent checkpoint.
