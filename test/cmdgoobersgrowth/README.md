# cmd/goobers growth review

CI measures each pull request's non-test Go lines and files at its head against
its merge-base with the target branch. Tests and child packages do not count.
A decrease or unchanged count passes. If either count grows, add a **new** file
`test/cmdgoobersgrowth/justifications/<issue-or-branch>.md` explaining why the
command package must grow and why the reusable work belongs there. Review that
rationale with the code. Existing or edited declarations do not authorize a new
PR. Use a unique filename; no absolute target or shared baseline edit is needed.

Run the same check locally after committing:

```
go run ./test/cmdgoobersgrowth -base-ref origin/main -head-ref HEAD
```

The checkout may be GitHub's synthetic merge: CI still counts the event's PR
head, so unrelated main changes cannot cause a failure. Merge groups compare
against their base and carry their constituent PR declarations. Pushes to main
and runs without `-head-ref` report counts without enforcing an absolute ceiling.

`baseline.txt` is an informational snapshot. The optional snapshot updater keeps
its explicit exact-target justification requirement, but snapshot freshness does
not affect PR enforcement. Do not update it in ordinary growth PRs.
