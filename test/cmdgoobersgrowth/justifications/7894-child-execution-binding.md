# Engine child execution binding (#7894)

`loadPinnedChildStage` is the command-layer owner that resolves a parent
stage's child authority from the parent's durable journal identity and its
pinned configuration generation. Engine children must carry the parent's
instance, repository and run-branch namespace into the start envelope at
acceptance, so the engine freezer can refuse a resolver that moves them.

`childExecutionBinding` derives that binding from the same two immutable
inputs the loader already holds. It is about fifteen lines and cannot live
below the command layer: only this owner has both the retained identity and
the pinned generation. It adds no command, flag or second authority path.
