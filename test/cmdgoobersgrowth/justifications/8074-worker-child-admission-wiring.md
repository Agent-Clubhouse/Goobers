# Worker child-admission client wiring (#8074, #7879)

The cloud engine worker needs the generated-child custody client in its
engine dependencies. The client and its constructor live in
`internal/childadmissionclient` (`ForWorker`). The command layer keeps only
the composition: one `stageDispatch.Children` field, with its comment, built
from the signing key and API endpoint that mode-3 dispatch already resolves,
and one `EngineDeps.EngineChildren` assignment.

That is five lines. It cannot live below the command layer, because only
this owner holds the dispatch seams, the supervisor-loaded key and the engine
dependency set together. It adds no command, flag or second authority path.
