# cmd/goobers growth: stage-bound child admission

LAND-C02b / HAW-CHD-002–003 connects the daemon's existing signing key, queue,
credential registrar, applied configuration reload, and execution-generation
archive to `internal/childworkflow`. The command package owns these concrete
services and its harness admission/instruction-loading helpers; the adapter must
preserve their retained definition-directory provenance and pinned digests.

Grant signing, transactional ownership, proposal validation, HTTP/MCP protocol,
policy-reload locking, custody checks, and retention remain in reusable internal
packages. The new command files only load and verify the daemon's retained
configuration and connect those existing services. No new CLI command, browser
identity system, child launcher, or general event framework is introduced here.

The public runner and engine continue refusing child-enabled execution. The
adapter tests deliberately exercise trusted journal/harness composition and do
not claim end-to-end parent/child execution support.
