# Control-role child-workflow routes for engine parents (#8085 c)

The `goobers-cloud control` role now serves the child-workflow routes
(validate, start, status, resolve) to engine parents' stage pods. All behaviour
lives below the command layer:
- `internal/cloud/childengine.FencedChildWorkflows` serves each request from
  the live lease epoch, re-resolving the parent's authority and checking the
  bound grant;
- the per-child generation pins are released at the engine bind, or at the
  tombstone of a child that was never bound;
- the cloud verifier accepts child-workflow grants (`internal/cloud/podauth`).

The command layer keeps only composition: `constructChildWorkflows`, a
controller-side seam registered after `EngineChildAuthority`, whose fenced
authority it reuses and without which it fails closed. It also passes the new
handler option to the control handler.

None of this can live below `cmd/goobers-cloud`. Only the role's composition
root holds the assembly registry and the seams it chains. It adds no command
and no second authority path.
