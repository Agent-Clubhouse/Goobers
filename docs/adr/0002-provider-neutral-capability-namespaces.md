# ADR 0002: Name capabilities for the operation surface

- Status: Accepted
- Date: 2026-07-28
- Decision owner: issue #1810

## Context

Provider-neutral PR stages introduced a namespace conflict. The Azure DevOps PR
work used `provider:pr:write`, while compile-time capability manifests proposed
`github:pr:write` for the same `open-pr` stage. Requiring every provider-specific
capability on a stage that dispatches through the configured provider couples
workflow definitions to instance configuration and grows the declaration for
every new provider. Treating the names as aliases instead would leave two
authoritative spellings for one authority and make credential admission
ambiguous.

Provider-specific operations still exist. GitHub review approval and Azure
DevOps PR status publication, for example, have different semantics and may use
different identities. Their names must continue to expose that distinction.

## Decision

Capability namespaces describe the operation a stage is authorized to perform,
not the credential implementation that ultimately performs it:

- A stage that selects the implementation from the invocation's configured
  repository provider uses `provider:<resource>:<verb>`, such as
  `provider:pr:write`.
- A stage that deliberately uses provider-specific semantics uses
  `<provider>:<resource>:<verb>`, such as `github:pr:review` or
  `ado:pr:status`.
- Provider-neutral and provider-specific spellings of the same resource and
  verb are mutually exclusive within one stage. Compilation rejects declarations
  such as `provider:pr:write` together with `github:pr:write` or
  `ado:pr:write`.
- Provider-neutral admission resolves only the configured repository provider's
  implementation and credentials. It does not inject credentials for every
  supported provider.

The names are not aliases. A provider-specific capability remains valid for an
operation that is genuinely provider-specific, but it does not satisfy a
provider-neutral stage contract.

## Migration

When a built-in becomes provider-neutral, its registry entry, command or
provider-stage manifest, policy-action contract, shipped workflows, examples,
and credential routing change together. A stale provider-specific declaration
fails validation with the required `provider:*` capability in the diagnostic;
the compiler does not silently rewrite it.

Already-started runs keep their pinned workflow definition and capability
declarations. Updated definitions use the new contract only for new runs, so no
runtime alias or dual grant is needed.

For the current conflict, provider-neutral `open-pr` and PR polling use
`provider:pr:write`, and their compile-time manifests require that exact
capability. GitHub-only PR review remains `github:pr:review`; other PR commands
move to `provider:*` only when their implementations actually dispatch through
the configured provider.

## Consequences

- Workflow authors do not enumerate providers for a provider-neutral stage.
- Adding a provider does not change existing provider-neutral workflow
  definitions.
- Provider-specific authority remains visible and independently grantable.
- Migration is intentionally fail-closed: old definitions must be updated
  instead of receiving an implicit capability alias.
- A command's namespace is evidence about its real abstraction boundary;
  describing a provider-specific implementation as provider-neutral is a
  contract bug.
