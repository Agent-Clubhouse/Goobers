# Schedule occurrence discovery wiring

The daemon gains two production lines to copy the authored `scheduleCatchUp`
policy into scheduler entries. Both entry construction paths need that value:
normal definitions and definitions retained while instructions refuse execution.
This preserves the same discovery policy during an admission refusal and when
the archived definition is reconstructed after a restart.

These assignments belong in the existing command composition root, where workflow
specifications become scheduler entries. Occurrence discovery, durable positions,
transactional capture and admission remain in their existing internal packages;
no reusable scheduling algorithm or additional production command file is added.
