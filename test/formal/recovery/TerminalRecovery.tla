-------------------------- MODULE TerminalRecovery --------------------------
EXTENDS Naturals

CONSTANTS Outcomes, MaxCrashes, TrustCheckpoint

VARIABLES outcome, pc, tail, logPhase, checkpoint, replayPhase, crashes

vars == <<outcome, pc, tail, logPhase, checkpoint, replayPhase, crashes>>
Running == "running"
ReplayPCs == {"replayed", "healed", "returned"}

Init ==
    /\ outcome \in Outcomes
    /\ pc = "ready"
    /\ tail = "none"
    /\ logPhase = Running
    /\ checkpoint = Running
    /\ replayPhase = Running
    /\ crashes = 0

BeginAppend ==
    /\ pc = "ready"
    /\ pc' = "partial"
    /\ tail' = "partial"
    /\ UNCHANGED <<outcome, logPhase, checkpoint, replayPhase, crashes>>

CompleteWrite ==
    /\ pc = "partial"
    /\ pc' = "written"
    /\ tail' = "complete"
    /\ UNCHANGED <<outcome, logPhase, checkpoint, replayPhase, crashes>>

SyncTerminal ==
    /\ pc = "written"
    /\ pc' = "synced"
    /\ tail' = "none"
    /\ logPhase' = outcome
    /\ UNCHANGED <<outcome, checkpoint, replayPhase, crashes>>

Checkpoint ==
    /\ pc = "synced"
    /\ pc' = "checkpointed"
    /\ checkpoint' = logPhase
    /\ UNCHANGED <<outcome, tail, logPhase, replayPhase, crashes>>

\* An unsynced complete line may survive, or be lost/torn. A synced line stays.
Crash ==
    /\ pc # "down"
    /\ crashes < MaxCrashes
    /\ \E survives \in (IF pc = "written" THEN {TRUE, FALSE} ELSE {FALSE}):
        /\ logPhase' = IF survives THEN outcome ELSE logPhase
        /\ tail' = IF pc = "written"
                   THEN (IF survives THEN "none" ELSE "partial")
                   ELSE tail
    /\ pc' = "down"
    /\ replayPhase' = Running
    /\ crashes' = crashes + 1
    /\ UNCHANGED <<outcome, checkpoint>>

\* TrustCheckpoint is a deliberately invalid, model-only recovery mutation.
Recover ==
    /\ pc = "down"
    /\ pc' = "replayed"
    /\ replayPhase' = IF TrustCheckpoint THEN checkpoint ELSE logPhase
    /\ tail' = "none"
    /\ UNCHANGED <<outcome, logPhase, checkpoint, crashes>>

HealCheckpoint ==
    /\ pc = "replayed"
    /\ pc' = "healed"
    /\ checkpoint' = IF replayPhase # Running THEN replayPhase ELSE checkpoint
    /\ UNCHANGED <<outcome, tail, logPhase, replayPhase, crashes>>

ReturnResume ==
    /\ pc = "healed"
    /\ pc' = "returned"
    /\ UNCHANGED <<outcome, tail, logPhase, checkpoint, replayPhase, crashes>>

Next ==
    \/ BeginAppend
    \/ CompleteWrite
    \/ SyncTerminal
    \/ Checkpoint
    \/ Crash
    \/ Recover
    \/ HealCheckpoint
    \/ ReturnResume

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ outcome \in Outcomes
    /\ pc \in {"ready", "partial", "written", "synced", "checkpointed",
                "down", "replayed", "healed", "returned"}
    /\ tail \in {"none", "partial", "complete"}
    /\ logPhase \in Outcomes \cup {Running}
    /\ checkpoint \in Outcomes \cup {Running}
    /\ replayPhase \in Outcomes \cup {Running}
    /\ crashes \in 0..MaxCrashes

\* Ordinary recovery must not reopen or change a journaled terminal outcome.
TerminalReplayStable ==
    (pc \in ReplayPCs /\ logPhase # Running) => replayPhase = logPhase

=============================================================================
