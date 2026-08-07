# Examples

Start with [`hello-world.yaml`](hello-world.yaml) when connecting a new
instance. Copy it into the target gaggle's `workflows/` directory, update
`spec.gaggle` if your gaggle is not named `example`, and trigger it manually
from the instance root:

```sh
goobers run hello-world
```

The workflow lists the configured backlog through `backlog-query --read-only`
with `github:issues:read`, then runs `make build` in the project checkout. The
read bypasses scheduler state and provider mutation paths; the workflow does
not claim or modify backlog items, run tests or lint, push code, or open a pull
request.

The [`ios-simulator`](ios-simulator/) example demonstrates a platform-specific
test workflow after the basic provider and build plumbing is working.
