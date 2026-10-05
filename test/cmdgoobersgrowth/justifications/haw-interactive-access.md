# HAW-HITL-001/002 daemon composition

The command package grows by one small composition file. It selects the daemon's
applied gaggle catalog, named instance credential sources, existing secret stores
and shared redaction registry, then registers the permission route and fences
catalog publication through the interactive policy service. These are host-owned
objects already assembled in `cmd/goobers`.

Policy validation, role and gaggle authorization, credential selection, provider
authentication, secret scoping and reload synchronization live in
`internal/interactiveaccess`. HTTP transport and contract code stay in their
existing internal packages. The command file contains no provider or policy
business logic. Existing OIDC constructors pass through the new group-claim
setting. Tests do not contribute to the measured command growth.
