# Child host publication

Command composition binds an accepted child's immutable source and workspace to
its current authority, configured repository and the existing credential plane.
Only canonical typed branch/PR commands route to the host publisher; authored
commands still use contained workers. This requires a command adapter and factory
selection, with no new CLI verbs or duplicated generic provider engine. Durable
intents, bounds, reconciliation and isolated Git transport live in internal
packages; GH/ADO effects reuse the existing provider implementations.

Tests run the production factory with real managed Git custody, ensure model
processes retain a model-only credential ceiling, and record confirmed effects
in the ordinary journal. Initial scope is one immutable branch and PR per child.
