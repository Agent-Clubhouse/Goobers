# HAW-CHD-008 execution epoch authority binding

The daemon adapter joins its existing durable child start envelope to a retained
journal and the current execution epoch. This small addition belongs beside the
private child execution reference and credential-plane composition: it does not
introduce a new authority format, queue, credential resolver or source loader.

Reusable epoch state, result custody and cancellation fences remain in
triggerqueue. Closed journal lineage and continuation validation remain in
journal. Human authority parsing remains in interactiveaccess. The adapter
explicitly separates historical custody from current effect authority so an old
signed execution cannot acquire fresh credentials after human restart. Runtime
restart activation remains deferred until the common admission/contained runner
and interactive credential paths compose. No growth baseline is repinned.

The same review adds the command adapter selecting existing interactive policy
leases and explicitly configured model API keys for child epochs. It branches
before automation injector construction, preserves signed model-only pod ceilings,
and selects the human repository binding for host publication. Journal, queue,
policy, credential resolution and worker lifecycle remain in existing packages;
the command owns only their composition and provider capability mapping. Lease
ordering follows the existing interactive-policy then child-authority ordering.
