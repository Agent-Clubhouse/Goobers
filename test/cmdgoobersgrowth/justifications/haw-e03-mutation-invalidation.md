# Connect uncached stage writes to shared-read invalidation

The GitHub stage factory attaches the mutation observer after constructing an
uncached provider. Reads keep their original live transport, credential refresh,
attribution, retries and quota owner. The existing scoped cache path is unchanged.

This adds nine production lines across the two existing command wiring files and
no production files. Mutation generation storage and transport decoration remain
in internal/apireadcache; provider effect detection remains in providers. The
command layer supplies the daemon-owned scheduler directory and explicit stage
scope, which the reusable packages must not discover from process environment.
