---
name: run-tests
description: Run the target repository's build and test suite and fix what the change broke before handing off.
---

# run-tests

Verify the change from the `implement` skill before the workflow's local-CI
stage and the `review` goober see it.

## Procedure

1. Identify the target repository's build/test entrypoint (Makefile,
   package manifest, or the gaggle's declared `ciCommand`).
2. Run it in the working tree.
3. Fix failures the change introduced; do not silence or skip failing checks
   to get a pass.
4. Report which checks ran and their result in the completion summary.

## Scope

Only fix regressions caused by this change. A pre-existing failure unrelated
to the claimed item belongs in a separate backlog item, not a silent fix
here.
