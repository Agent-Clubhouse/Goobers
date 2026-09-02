<!-- Thanks for contributing to Goobers! Keep PRs small and scoped. -->

## What & why

<!-- What does this change do, and why? Link the issue it closes. -->

Closes #

## Changes

<!-- Bullet the notable changes so a reviewer can scan them. -->

-

## Checklist

- [ ] Scope is limited to one logical concern (no unrelated changes)
- [ ] New behavior and error paths have tests
- [ ] Any new append-only file, table, directory, or long-lived map has a
      bound/pruner wired by a production caller, with a test that asserts the
      bound ([review rules](../CONTRIBUTING.md#append-only-growth-needs-a-wired-bound-or-pruner))
- [ ] Any advertised recovery exit (unpark, rollback, self-heal, escape hatch)
      has a production caller registered in this change and a test that invokes
      the recovery path
      ([review rules](../CONTRIBUTING.md#advertised-recovery-exits-need-a-registered-tested-caller))
- [ ] `make ci` passes locally (fmt-check · vet · build · test · lint)
- [ ] Any new closed JSON schema joins the structural drift guard in this change
      ([review rules](../CONTRIBUTING.md#review-rules))
- [ ] Docs/comments updated where behavior changed
- [ ] For security-sensitive changes, I followed [SECURITY.md](../SECURITY.md)

## Notes for reviewers

<!-- Anything that needs special attention, trade-offs, or follow-ups. -->
