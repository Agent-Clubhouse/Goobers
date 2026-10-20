# Copilot parent credential boundary

The command-layer change separates existing kit credential filtering from the
extra generated-child authentication restriction. Parent kit construction may
retain a Copilot model grant already held by its stage, while generated children
continue to reject that inseparable credential. These few lines belong beside
the existing parent/child kit writers and do not introduce a new CLI service or
owner. The parent harness allowlist also adds Copilot without changing placement.
