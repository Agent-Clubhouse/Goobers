# HAW-CHD contained agent telemetry scope composition

This PR adds one small helper beside the daemon's existing signed child-journal
adapter. The adapter must compare typed harness lifecycle and progress scopes
with the exact authenticated contract before filling an absent outer stage.
A foreign nested run, stage or attempt remains a refusal, as does a conflicting
outer scope. The helper returns a projected event without changing its input.

This belongs with the command package's existing child-journal boundary because
it composes the daemon's stage naming, contract authority and refusal behavior.
It introduces no new journal format, store, credential authority or general
harness policy. Those reusable owners remain in internal/journal,
internal/childpod and internal/harness. Moving the helper alone into a new package
would move the daemon's adapter policy without moving its actual owner.
