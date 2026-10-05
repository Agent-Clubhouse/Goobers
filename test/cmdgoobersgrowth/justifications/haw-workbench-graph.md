# Authorized relationship graph installation

The daemon's existing workbench composition now installs the bounded graph route
alongside its existing source reader. Authorization, source aggregation, conflict
handling and projection remain in internal packages; the command package adds no
new reader or planning logic. A composed host test verifies current repository
credentials and that revoked content is excluded from graph reads. No baseline
changes are requested.
