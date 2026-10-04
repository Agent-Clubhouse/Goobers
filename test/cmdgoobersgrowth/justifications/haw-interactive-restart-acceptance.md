# Interactive restart composed acceptance

This PR adds a private optional `harness.ProcessRunner` dependency to the existing
interactive restart composition. Production always supplies nil and keeps the
normal harness process implementation. The daemon package owns the pinned
archive, source policy, provider, runner and HTTP assembly under test; the seam
reuses the harness's existing process interface instead of adding a fake agent
execution path or bypassing credential delivery and sandbox policy construction.

The composed acceptance test covers HTTP guidance and restart, real configured
human credential selection, provider source checks through an in-process HTTP
transport, fresh stage retry allowance, source journal preservation, duplicate
epoch replay, interrupted epoch recovery and current-policy revocation. The fake
model starts no operating-system process and makes no native termination claim.
