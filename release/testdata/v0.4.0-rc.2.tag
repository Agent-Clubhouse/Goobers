object a1b2ae99ea8ee0874517560e42bfaeee7c9f95e6
type commit
tag v0.4.0-rc.2
tagger scratch <scratch@local> 1789111234 -0700

v0.4.0-rc.2 — release candidate

Second release candidate for v0.4.0, superseding v0.4.0-rc.1. This is a
pre-release: it is never promoted to "Latest release", and install.sh
intentionally refuses pre-release tags — download and verify an archive
directly to test it.

Every item tracked against this release is now closed.

Highlights since rc.1

Retention defaults. Worktree and branch pruning now ship opt-out with a
safe first-enable grace window, completing the change whose telemetry
half shipped in rc.1. An instance left at stock settings no longer
accretes run data and local branches indefinitely — the gap that made
rc.1 an incomplete candidate for promotion.

Security. The portal escapes run and instance metadata before it
reaches innerHTML, closing a stored and reflected injection path
through repository-controlled names and identifiers. The monitoring
exporter image pins its base and hash-locks its dependencies, so
rebuilds no longer drift across operating-system and interpreter patch
levels.

Windows. Validation waits for daemon readiness rather than racing it, a
disruptive WSL fixture is isolated from the rest of the suite, and the
quickstart states the native Windows versus WSL 2 host route up front
instead of leaving the choice to inference.

Harness. Forwarding launchers are verified before use.

Known issues and support boundaries

install.sh refuses pre-release tags by design; verify and extract an
archive manually to evaluate this candidate. Publishing a container
image is still outstanding, so the reference Kubernetes deployment
requires building your own image.
-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAgdjKR5gy+aDMtEgxvmgby/bgVRj
V53pSVfBPrYQ3RRD0AAAADZ2l0AAAAAAAAAAZzaGE1MTIAAABTAAAAC3NzaC1lZDI1NTE5
AAAAQCMzK25dd1LU2eEY3cPjjVunW1C6df5PSIpI6RJ1faFFuuLZJ6sjhvHjv1icPS1rWm
pzZX7Mknl1GbdLGsDDLgo=
-----END SSH SIGNATURE-----
