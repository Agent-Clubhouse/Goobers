# Copilot stage authentication and configuration isolation

When a stage receives a resolved `agent:model` credential, Goobers gives Copilot
an invocation-private `COPILOT_HOME`. Environment and keychain tokenRefs both
follow this path: the credential resolver supplies the model token before the
process starts. Ambient hooks, plugins, instructions and MCP configuration are
not copied. Workspace MCP auto-discovery is disabled for this path. Completion
repair keeps the same invocation home; the next stage invocation gets a fresh
one. Native transcript capture finishes before the home is removed.

Goobers never copies Copilot login files into this home. It also never copies
`config.json` into an authentication preflight home.

For compatibility with ordinary `copilot login`, an unconfined stage without a
resolved model credential retains the existing CLI home. Preflight prints a
warning that configuration isolation is reduced and recommends configuring
`agent:model` through an environment or keychain tokenRef. This path can load
ambient CLI customizations; it is not an isolated stage. Repository `GH_TOKEN`
grants do not count as model credentials and cannot silently select this policy.

An enforced sandbox or declared external MCP configuration keeps its existing
isolated execution path and never falls back to the ambient home to recover
authentication. External MCP already requires a resolved model credential.
File-only login cannot satisfy an isolated runtime; supply `agent:model` instead.
CLI-owned OS authentication that remains available without importing a login
file is left to the CLI, but is not assumed to be present or usable.

Preflight's working directory stays private in both modes. With a resolved model
token its CLI home is private too; stored-login compatibility uses the original
home and emits the warning. A successful version probe alone does not establish
model authorization.
