# Scoped workflow sources and Goober discovery

The command package grows to connect repository-relative Git workflow sources
to instance startup, reload, config-repository checkout, and publication. The
selected directory must remain the boundary for materialization and writes,
and its URL, directory, and commit must survive reload and startup restoration.
Those are command and daemon lifecycle responsibilities, while path validation,
Git tree selection, and provenance data remain in reusable internal packages.

The related Goober-discovery fix uses the shared recognition rule from command
runtime resolution instead of treating arbitrary sibling directories named
`goobers/` as definitions. Keeping the narrow adapters at these existing
command entry points avoids duplicating source-selection or discovery logic.
This declaration covers only the command-package growth for issues #8279 and
#8280; it does not change the informational snapshot or any growth threshold.
