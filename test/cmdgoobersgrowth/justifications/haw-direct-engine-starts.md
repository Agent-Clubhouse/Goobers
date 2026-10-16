# LAND-E01: direct engine starts

The command package already owns engine-start and daemon queue wiring. The new
adapter binds these existing owners to internal/enginestartintent; canonical
input validation, exact transport binding and Temporal history comparison live
in that dedicated package. The existing triggerqueue owns atomic storage and
migration. There is no new daemon, queue database or credential provider.

The command tests verify the real CLI, queue reopening and daemon drain. A
separately declared integration test uses a disposable Temporal frontend to
prove recovery after a successful remote start reply is lost. This declaration
covers only the direct-start adapter and does not reset a growth baseline.
