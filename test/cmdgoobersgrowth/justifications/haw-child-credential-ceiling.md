# haw-child-credential-ceiling: command composition review

The daemon credential broker must bind mint/refresh to the exact accepted child and current runtime lease, then recheck cancellation after minting. The command adapter uses its existing queue, journal and credential-plane ownership. Credential ceiling calculation, materialization filtering and trusted runner pins remain in internal packages.

Stable HAW design tasks are the planning references until numbered backlog items
are created after review. This declaration applies to this PR against its stacked
base; the shared baseline is deliberately unchanged.
