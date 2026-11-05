# #7761: Apply remaining allowance in the pod supervisor

Growth: +34 non-test lines and +0 production files in `cmd/goobers`.

The pod supervisor now consumes the per-attempt allowance from the verified
agentic kit, refuses an explicit exhausted allowance before harness startup,
and evaluates adapter snapshots against remaining rather than original limits.
The kit writer validates the trusted allowance before publication.

The deterministic allowance calculation and engine retry transport live in
internal packages. These command-package lines are the necessary trusted
supervisor and kit-composition boundaries.
