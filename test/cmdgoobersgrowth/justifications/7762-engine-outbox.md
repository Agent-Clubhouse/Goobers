# #7762: Export declared outbox files from the pod supervisor

Growth: +97 non-test lines and +1 production file in `cmd/goobers`.

The new file is the trusted `__dispatch-exec` outbox collector. It applies the
runner's containment and aggregate limits, scrubs files with the attempt's
credential-aware scrubber, uploads the exact scrubbed bytes by digest, and
returns only a bounded path/digest/size manifest for surrender.

This logic belongs in the command package because the pod supervisor owns the
workspace after stage completion and holds the short-lived blob-plane
credential. Moving it into the workflow would put file bytes in Temporal
history, while moving it into the stage process would let workflow-authored
code choose or forge its own durable evidence.
