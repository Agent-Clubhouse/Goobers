# Azure DevOps limitations

DSL 2.0 supports Azure DevOps (ADO) without new DSL vocabulary and without
changing GitHub or Gitea behavior. Some scopes are intentionally out of
reach for v0.5.0; this guide lists them so a gaggle fails validation with a
clear reason instead of misbehaving at run time.

## Mixed-provider backlog and project (topology b)

A gaggle's `spec.backlog.provider` must match its `spec.project.provider`
today. `validate` refuses a gaggle where the two differ and either side is
`ado` (for example a GitHub backlog with ADO code, or an ADO backlog with
GitHub code): every backlog stage currently opens the routed *project*
provider, so a mismatched backlog would silently query the wrong forge
instead of failing loudly.

A mismatch between two non-ADO providers (for example a GitHub project with
a Gitea backlog) is reported as a warning rather than refused, so an
existing non-ADO configuration does not break.

An Azure DevOps project split — the project repository in one ADO project
and `spec.backlog.project` naming a different ADO project — is unaffected
and keeps working, because both sides share the same `ado` provider.

Full mixed-provider backlog/project support (topology b) is planned for a
v0.5.x release. It routes each backlog stage by role, binds credentials by
capability family, and stops writing cross-provider closing references. See
`docs/design/ado-parity-dsl-2-0.md` §7.2 for the design.

## One gaggle, two code providers (topology c)

A single gaggle coordinating code repositories on both ADO and GitHub is
DSL 3.0 scope (`docs/design/provider-access-layer.md`), not this plan.

## Azure DevOps Server (on-premises)

Only `dev.azure.com` is supported, plus legacy `*.visualstudio.com` URLs
where that is cheap. Azure DevOps Server (on-premises) is out of scope.

## Service-hook triggers

Goobers polls Azure DevOps; it does not consume ADO service-hook events.
