# Deterministic stage retry CLI adapter (#8269)

`goobers retry-stage` is a thin command-layer adapter over the existing
escalated-run rerun intent. It parses the run, stage, actor and expected
terminal sequence, then calls the same runner/HTTP mutation path as
`rerun-stage` with the deterministic-retry intent (no addendum). Request
validation, escalated-run checks, custody, walk-state seeding and HITL attempt
classification all live in internal/runner and internal/engine; the command
package only adds flag parsing, registration, help text and capability
advertisement, so this growth cannot move into a reusable package.