# Control-role claims plane composition (#7818)

`goobers-cloud control` composes the claims plane from the PostgreSQL plane in
internal/cloud/claims. The new constructor only wires that owner to the
controller database and lease. Every claim transition, fence, epoch adoption
and recovery rule lives in internal/cloud/claims with its integration tests, so
nothing here adds a second claim path. It lives in its own file so the manifest
in controlassembly.go keeps one line per seam.
