# #4768: local/self launch receipt wiring

This PR adds command wiring for authoritative instance-key admission, local
receipt persistence, ephemeral local start authority, and doctor/status guidance.
The command layer reads the instance configuration before selecting archived
workflow settings or preparing local executors. Receipt validation, single-use
local authority, immutable persistence, canonical start binding, and refusal of
runner entry points and executor factories live in internal packages.

No new production command files or command families are added. Every current
local/self execution kind is refused when `api.podTokenKeyFile` is configured;
there are no builtin exemptions pending #6532. Shared diagnostics disclose the
configuration key, not a host filesystem path. This is private prepared-launch
receipt wiring and makes no public attestation claim.

The comparison with PR2A also includes the already-reviewed #6524 signing-key
guard prerequisite, whose command changes only connect the internal credential
path inventory to agentic construction. The absolute growth baseline is unchanged.
