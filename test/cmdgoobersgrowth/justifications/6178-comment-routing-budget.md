# PR comment watcher routing budget (#6178)

The command gains a provider-neutral routing-budget record and small forge
adapters for publishing it and applying a human hold. This state belongs beside
pr-comment-watch: it governs that stage's routing decision, uses its existing
comment classifier and provider seam, and is persisted on the PR so local and
pod executions share the same history. It does not add another storage backend
or duplicate provider HTTP implementations.

Unmarked shared-identity feedback remains human. The watcher reserves at most
three routing attempts per PR head, then records a visible escalation and parks
the PR. This bounded recovery replaces an unsafe authorship guess based on when
the comment was posted. The budget parser and rendering are isolated from the
scan orchestration; generic provider behavior remains in providers.
