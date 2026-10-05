# HAW-HITL-007/010: post-turn PR repair observation

The daemon installs the common retained repair observation service with its
existing queue, interactive permission service and scoped provider factory.
The installer contains no provider mutation or duplicate recovery logic. HTTP,
ledger proof, bounded history and retention live in their existing packages.
The portal explicitly loads and checks receipts after a session turn closes.
