# Shared run semantics extraction for HA-P3-18

The command's existing calls to shared runner helpers now import
internal/runsemantics. Files that also construct a local runner need both
imports. This is mechanical dependency separation, with no new CLI behavior,
flags, construction or policy. Implementations and pure unit tests move to the
neutral package; local runner and Temporal engine consume the same definitions.
