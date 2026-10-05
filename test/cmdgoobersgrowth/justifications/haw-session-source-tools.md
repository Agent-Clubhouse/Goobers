# Native session source tool composition

The command package adds one adapter file that binds a real session journal,
its exact initiating human, current interactive execution lease, and configured
source-reader factory to the built-in MCP runtime. This daemon composition needs
the existing runner registry, credential endpoint, and profile archive and stays
beside the other human execution adapters. Provider-neutral requests, bounded
transport, grant ownership, authorization checks, and operation evidence remain
in internal packages. Native sessions now pass the existing executable to the
built-in MCP registration; authored external MCP and raw provider credentials
remain unavailable to the model process. No shared growth baseline is changed.
