# Control role run-journal composition

`constructRunJournal` wires the control assembly's RunJournal seam, the last
read seam the control role needs before its assembly can complete and serve.
Like every other assembly constructor it lives beside the seam table in
cmd/goobers-cloud: it only composes existing owners. The cross-run read plane
is `planes.RunJournalService` (shared with the daemon), journal reads are the
journalpg reader the Journal seam builds, claims are the claims plane, and
escalation reads are the cloud Reader. The command-layer additions are the
constructor, the applied-generation workflow resolver and two plumbing hooks
on the assembly's plane set. They add no flag, command or lifecycle.
