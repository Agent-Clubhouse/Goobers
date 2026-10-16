# cmd/goobers growth: child custody and durable waits

LAND-C03 and the LAND-C04 lifecycle foundation connect the daemon's retained
configuration loader, runner registry, cancellation owner, scheduler, trigger
reconciler, credential broker and repository identity adapters. These concrete
owners currently live in the command package. Their adapters must hold the same
reload, runtime-generation and custody leases across queue publication and crash
recovery; disconnected library helpers cannot establish those lifetimes.

Snapshot/fork/result storage, bounded queue transitions, scheduler accounting,
writer quiescence, runner wait projection and continuation remain in internal
packages. Command additions bind those reusable services to the actual daemon
lifecycle, including recovery and cancellation after a receipt is dispatched.
No new CLI command, provider mutation, browser authentication or Portal surface
is introduced. Complexity limits and the growth baseline are unchanged.

The internal execution driver requires explicitly isolated factories. The shipped
launcher defers when that backend is absent, and public child workflow execution
stays refused. This boundary does not deliver result disposition or a supported
public parent/child journey. Those remain separately reviewed follow-ups.
