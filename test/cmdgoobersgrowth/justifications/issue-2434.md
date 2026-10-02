# Scheduler setup ownership (#2434)

Three command-local files separate observability/read projection, scoped claim
recovery, and per-gaggle runtime construction from the daemon composition root.
Their ownership and rollback logic belongs in cmd/goobers because it composes
private command wiring and instance lifecycle policy; it introduces no reusable
store, runner, or scheduler behavior. Named input bundles replace the long
positional argument lists. The extra files make the resource boundaries visible
and allow construction failures and normal shutdown to use the same cleanup.
The existing complexity and body-length entries for setup are removed after the
composition root falls below both caps; no complexity budget is increased.
