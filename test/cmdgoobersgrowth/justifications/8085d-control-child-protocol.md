# #8085 (d): compose the engine child protocol in control

`cmd/goobers-cloud` grows by about 31 non-test lines:
- an `EngineChildProtocol` controlPlanes field and its assembly row;
- the one constructor that builds `childengine.FencedProtocol` over the control-side journal backend;
- passing the new handler option to the API server.

All the protocol logic lives outside `cmd`:
- `internal/cloud/childengine`: the Finalize/Park/Complete service, `FencedProtocol` and the parent capacity plane;
- `internal/enginechildplane`: the HTTP adapter;
- `internal/httpapi`: the routes;
- `internal/childadmissionclient`: the worker client.

The constructor follows the existing engine child authority constructor beside it: same dependencies, same fail-closed validation. Splitting it out further would add a package boundary for one wiring function.
