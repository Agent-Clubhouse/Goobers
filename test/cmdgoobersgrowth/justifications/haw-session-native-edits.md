# Shared session native edit installation

The daemon wires the existing durable workbench writer into the live session
runtime alongside source reads, carrying the exact initiating actor and retained
source configuration. A narrow factory returns no writer when editing is absent,
so model/read-only sessions remain usable. Typed effects, current lease checks,
command custody and actual result journaling remain in internal packages; command
code only composes those services and their per-invocation tool access. No
complexity or growth baseline is widened.
