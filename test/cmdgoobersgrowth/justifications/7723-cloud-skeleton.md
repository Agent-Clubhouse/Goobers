# HA-P1-13 cloud command skeleton

Issue #7723 intentionally introduces `cmd/goobers-cloud` as the cloud
control-plane composition root. Its registry, four role commands, generated
documentation entrypoint, and `internal/workerhost` adapter belong in this
binary rather than `cmd/goobers`; reusable behavior remains in `internal/`.
