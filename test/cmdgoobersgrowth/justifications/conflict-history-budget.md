# fix/conflict-history-budget

cmd/goobers grows by roughly 35 non-test lines in `implementcontext.go`: a
`planeReadDegradable` classifier, two artifact fields and the degrade branch in
`gather-implement-context`.

Why it must live in the command package: the degrade decision is part of the
`gather-implement-context` stage command's contract (exit code and the shape of
its `implementation-context.json` artifact), and it needs the stage's own
backend selection (`stageCrossRunJournal`). The reusable work, pruning the
cross-run scans by journal mtime before opening journals, is in
`internal/journalclient/file.go`, not in the command package.
