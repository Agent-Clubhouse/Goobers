# Examples

Start with [`hello-world.yaml`](hello-world.yaml) when connecting a new
instance. Copy it into the target gaggle's `workflows/` directory, update
`spec.gaggle` if needed, and run it manually. The workflow only lists the
configured backlog with `github:issues:read`, then runs `make build` in the
project checkout. It does not claim or modify backlog items, run tests or lint,
push code, or open a pull request.

The [`ios-simulator`](ios-simulator/) example demonstrates a platform-specific
test workflow after the basic provider and build plumbing is working.
