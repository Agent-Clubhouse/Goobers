# NetworkPolicy drift gate fixture

This is a synthetic, offline CI fixture, not a deployable instance. Its
sample public IP ranges and fake upstream snapshot must not be used as
a production allowlist. It covers allowlist, no-network and unrestricted runner
classes, with a committed address-count baseline and rendered manifests.

`make deploy-validate` runs `TestDeployReferenceNetpolCommittedDriftGate`. The
test invokes the real `netpol-render --check` command against these committed
files and substitutes `upstream-meta.json` for the provenance fetch. It does
not regenerate either the manifests or baseline before checking. Negative cases
prove inventory/output changes, a lower coverage baseline, a missing baseline,
and drift in the second provenance marker fail the gate.

After an intentional renderer or fixture change, review and regenerate from
the repository root:

```sh
go run ./cmd/goobers netpol-render --out deploy/reference/examples/netpol-drift/rendered --write-baseline deploy/reference/examples/netpol-drift
go test ./cmd/goobers -run '^TestDeployReferenceNetpolCommittedDriftGate$'
```

If changing `upstream-meta.json`, update each `sourceSHA256` marker to the SHA-256
of that file's exact bytes. Review address-count baseline increases explicitly.
Production overlays still run `goobers netpol-render --check --out <rendered-dir>
<instance-root>` against their real upstream sources; this deterministic CI test
does not claim to detect a live provider rotation.
