# HAW-EVT-007: exact shared-turn cancellation observation

The daemon adds a small typed-source dispatch and records the actual terminal
journal time in its existing session observer. The host owns journal and runtime
writer evidence; the reusable turn cancellation coordinator remains in
internal/interactivesession. It cancels one owner without closing the shared
conversation or borrowing another turn's state. No policy lock is reentered.
