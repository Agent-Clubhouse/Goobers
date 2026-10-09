# cmd/goobers growth: parent result disposition

LAND-C05 adds the daemon handoff adapter for an exact parent disposition request.
The existing daemon owns current stage policy, the credential exclusion mapper,
retained definition leases and parent workspace custody. Those owners must remain
held while checking the accepted result and applying the retained plan.

Git tree planning/application, queue receipts, revision history and CAS semantics
remain in reusable recovery/childworkflow/triggerqueue packages. The command code
only binds them to the real stopped-writer handoff and returns bounded preparation
or reconciliation outcomes to the runner. This is an extension of the preceding
custody adapter, not a separate execution framework or new CLI command.

No baseline is raised, and public child execution remains refused until the
isolated backend is qualified. PR publication and Portal disposition UI are
separate delivery slices.
