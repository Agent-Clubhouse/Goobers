# Interactive DSL 3.0 sample

This normal configuration directory contains a Manifest, Gaggle, two existing
Goobers with Markdown instructions, and a Workflow. The experimental portal host
loads it through `instance.LoadConfigDir`; it is the source of chat configuration,
not a separate surfaces file. See the
[launch guide](../../docs/guides/interactive-prototype.md) for building and running
the host. No model or provider call occurs during config validation.

The Gaggle exposes `helper` and `workbench`; runtime IDs are
`interactive--helper` and `interactive--workbench`. Both bind their named Goober
and the same `invoice-check` workflow. The helper permits reviewed management
proposals; workbench focuses on repository investigation, editing and children.
Surface tools, workflow references, authority references, execution modes,
session limits and child command capabilities are declared in `gaggle.yaml`.
Omitted tool, workflow and authority lists permit none.

`Gaggle.dslVersion: "3.0"` pins only `spec.interactive`. The Gaggle acknowledges
that preview itself. Each Workflow retains its independent version and preview
acknowledgement; a Gaggle cannot authorize a Workflow's preview pin. Legacy
Gaggles without the block are unchanged, and `interactive: {}` exposes no chat.
Interactive children use a bounded native workflow adapter, distinct from
DSL 3.1 `task.childWorkflows` and its production lineage/admission machinery.

Both surfaces request `codex-local` by default. Without independently enabled
trusted local model access, that request fails explicitly. Select
**Deterministic demo** (`local-tools`) in the portal for local execution without
a model. Session mode cannot change after creation. The existing Goober harness
is `codex`; `model` is omitted to retain the adapter default. Operators may set
an approved model in the Goober. The live model smoke remains unexecuted after
an automatic approval-review denial of the sample/model disclosure; this sample
does not authorize that transmission or establish a successful native model run.

Surface `tools` allow named mediated host MCP operations. They are different
from `Goober.spec.tools` and do not restrict the native harness's builtin
filesystem/shell tools. The native path trusts the local workstation boundary.
The prototype compiler rejects unsupported Goober tools, external MCP servers,
harness options, skills and capability/policy grants rather than silently
inheriting them. This sample declares none.

The ADO tuple names controlled local fixture context. `controlled-local-ado` and
`controlled-local-app-insights` are opaque host broker lookup keys. They create
no credentials, connections or grants, and do not establish live Microsoft
access. The broker binds actual identity/resources/actions at execution time;
no provider token belongs in this configuration, instructions, prompts or output.
There is no application-identity fallback. Native Azure CLI and token-consuming
MCP tools do not transparently inherit mediated authority.

The host hashes and snapshots the configuration plus any adjacent shared Goober
instruction tree into private state. It supports YAML and Markdown assets here.
After editing a copied configuration, explicitly POST to
`/api/v1/interactive/config/reload`. Invalid input retains the previous catalog.
A changed valid revision fences old sessions, waits for writer custody and
requires a new session; it never silently widens existing authority. Read the
launch guide for the complete reload command and operational boundary.

Session values are ceilings which the worker can narrow. Coordination uses one
exclusive local controller with durable leases, not distributed multi-writer
storage. Child proposals retain exact reviewed YAML and require human approval
at an idle parent checkpoint. `invoice-check` uses DSL 3.0 scratch execution and
`runsOn.restrictions: [network:none]`; the expected `invoice.test.mjs` comes from
the host's separate trusted `--source` directory, normally
`samples/interactive-investigation`. No remote checkout is implied by metadata.

Normal schema/loader/CRD and fake-transport apply tests exercise this sample.
Those tests do not qualify live model behavior, live Azure authentication or
production deployment. The final prototype handoff must record the exact
integrated commit and current browser/native execution evidence separately.
