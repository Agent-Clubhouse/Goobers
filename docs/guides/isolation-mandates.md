# Instance isolation mandates

The optional `instance.yaml` isolation floor requires matching stages to run on
runners that already enforce the named effects. It does not grant protections,
rewrite workflows, migrate gates, or deploy networking.

```yaml
isolation:
  mandates:
    - match: {stageClass: agentic}
      restrictions: [network:allowlist, tmp:ephemeral]
```

The selector vocabulary is `agentic` (agentic tasks and reviewers) and
`deterministic` (deterministic tasks and non-agentic control-plane gates).
The effects are the same closed list used by `runners[].restrictions`:
`network:none`, `network:allowlist`, `fs:readonly-except-workspace`,
`tmp:ephemeral`, and `env:default-deny`.

All mandates matching a class are unioned with the stage's requirements. A
gaggle or task cannot remove the floor. Validation rejects unknown selectors,
unknown or duplicate effects, empty mandates, and a combined class floor no
runner satisfies. Workflow validation and runtime admission additionally check
each stage's capabilities, OS and restrictions. A diagnostic names the stage
and missing effect; changing a stage requirement cannot relax an instance mandate.

Unplaced reviewers and other control-plane gates remain local: they cannot
borrow a remote runner's protection. A covered unplaced gate therefore refuses
if no self runner satisfies the floor. Declare supported agentic gate placement
in DSL 3.0 to dispatch a reviewer; adding a mandate alone never adds `runsOn`.
Likewise, if whole-workflow engine selection falls back to the daemon, local
admission still requires the floor. There is no silent downgrade.

Omitting `isolation` preserves existing placements and validation behavior.
Mandates follow the startup-pinned instance inventory: restart after changing
the instance policy, following normal drain/restart procedures. Existing runs
retain their pinned placements; this is not retroactive migration or revocation.
`goobers status` displays configured class floors and its JSON output includes
`isolationMandates`; refused workflows retain their placement diagnostics.
`goobers explain instance.isolation` describes the configuration contract.
