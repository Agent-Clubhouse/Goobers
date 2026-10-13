# LAND-E01: durable signal and webhook starts

The command package owns signal CLI lifetime and daemon webhook wiring. This
adapter connects those owners to internal/startintent and the existing queue.
Recipient selection and admission stay in internal/localscheduler; atomic
receipt storage and its additive migration stay in internal/triggerqueue.

One command helper retains only the selected receipts, observes actual run
publication, and reports capacity-held starts with their retry identity. The
connected tests prove HTTP acknowledgment, restart before dispatch, execution
of the captured definition and replay without another run. This declaration
covers this adapter only and does not reset a growth baseline.
