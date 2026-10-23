# #7761: Enforce trusted usage in the pod supervisor

Growth: +119 non-test lines and +1 production file in `cmd/goobers`.

The new file is the pod supervisor's trusted usage controller. It accepts only
canonical adapter callbacks, retains the latest cumulative snapshot, cancels
the harness context with a typed cause at token/cost caps, and freezes the
first over-cap evidence for surrender. `dispatchagentic.go` installs it around
the harness; `dispatchexec.go` carries only its snapshot into surrender.

This logic belongs in the command package because `__dispatch-exec` is the
trusted process supervising the harness. Moving it into a harness adapter
would let adapter-specific code own policy and duplicate enforcement, while
moving it into the engine would be too late to terminate the in-pod process.
