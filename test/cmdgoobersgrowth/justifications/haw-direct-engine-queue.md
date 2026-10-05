# HAW-EVT-002: direct engine custody composition

The command package replaces its direct Temporal call with the shared ledger's
typed direct-engine admission service. Its adapter selects the existing archived
compiler, configured Temporal dial and payload codec, and preserves CLI output.
The daemon installs that same service and a bounded independent sweep. These are
host composition duties; closed envelopes, input digests, credential-selector
binding, exact Temporal observation, and durable transitions live in the internal
enginestartintent and triggerqueue packages. There is no alternate metadata store.

The new command tests drive both the real direct CLI and delegated engine file
path, replacing only the Temporal network client. The reusable provider identity
and custody behavior is tested independently of the command package.
