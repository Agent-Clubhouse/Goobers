# #5293 worker surrender transport

The CLI wiring grows by 16 non-test lines and no new Go files to select the
worker-only surrender read client in blob-endpoint mode, while retaining the
directory-backed plane in directory mode. Authentication, bounded HTTP reads,
and filesystem confinement remain in the existing internal packages. This
completes removal of shared artifact mounts for dispatch workers.
