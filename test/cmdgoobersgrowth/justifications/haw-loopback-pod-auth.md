# LAND-C06 loopback machine authentication wiring

The daemon adds a small composition block to its existing HTTP startup owner.
It must combine the configured pod verifier and active grant key with local
trusted administration only when the listener is loopback and no OIDC issuer
is configured. Machine-principal authorization is reusable and lives in
internal/httpapi; token parsing and verification remain in internal/podauth.
No user identity store, login flow or new credential format is introduced.
