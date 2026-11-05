# Bounded lineage display (#7456)

The CLI gains ten non-test lines to label truncated lineage, partial ancestor
counts, and incomplete continuation history in `goobers trace` output. These
are presentation adapters for the bounded read results; the traversal, cursor
paging, and indexed queries live in internal packages. Showing these limits
prevents a bounded response from being presented as complete history.
