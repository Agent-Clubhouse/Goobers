# haw-child-pinned-admission: command composition review

Pinned-stage admission needs the daemon's immutable config archive, current applied catalog and generation lifetime. These adapters supply those host facts to internal/childworkflow and fence reload publication. The reusable admission policy and signed grant checks stay outside the command package.

Stable HAW design tasks are the planning references until numbered backlog items
are created after review. This declaration applies to this PR against its stacked
base; the shared baseline is deliberately unchanged.
