# cmd/goobers growth: issue #6328

The opt-in intake duplicate/dependency check needs one hook in
`runClaimBacklogQuery` and a small annotation file in `cmd/goobers`, because
the claim result, stage annotator and instance layout live in this command.
Candidate retrieval, similarity, verdict logic and settings live in
`internal/decisiongate` and `internal/runtimewiring`; the command only
journals the advisory result and never alters the claim.
