# #7872 prerequisite: consume prepared reviewer evidence in stage pods

`cmd/goobers` gains nine non-test lines and no files. Its existing
`__dispatch-exec` review adapter must recognize the engine-authored optional
prepared-diff field after context materialization, return a mechanical verdict
or refusal when appropriate, and bypass legacy pod capture and publication.
The validation and verdict rules live in `internal/runsemantics`; only the
adapter to `stageOutcome` and the existing legacy-path guards remain in cmd.
The shared snapshot is unchanged.
