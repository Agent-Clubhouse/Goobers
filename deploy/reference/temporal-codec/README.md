# Temporal history keys in the reference deployment

The authenticated reference generator enables `temporal.payloadCodec` by default,
using the existing Secret `goobers-temporal-codec-key` in `goobers-system`. This is
an opt-in reference topology; local `goobers init` defaults remain plaintext.

After creating the namespace, provision the dedicated RSA key once:

```sh
config-examples/temporal-codec/provision-key.sh goobers-system goobers-temporal-codec-key
```

The script uses OpenSSL's RSA key generator, then `kubectl create secret` and
refuses an existing Secret. It never updates or applies over existing key material.
Its temporary directory is private and removed on exit. Back up this Secret with
Temporal's PostgreSQL backups; a database backup without its keys is insufficient.
Kubernetes Secret storage should use the operator's normal encryption-at-rest and
RBAC policies.

Run the [authenticated generator](../authenticated/README.md) normally, or name a
separately provisioned key with `--temporal-codec-key-secret <secret>`. It adds a
`file-key` store and keyRef to the immutable instance configuration shared by the
daemon and worker, with `strict: false`. The Secret must contain `active` and every
retained `<version>.pem`; initial provisioning writes `active=v1` and `v1.pem`.

The existing init container copies projected Secret files into a private memory
volume, owned by the workload UID: directories 0700, regular files 0600. This is
required because `file-key` refuses projected symlinks and group-readable keys.
Only the daemon and Temporal worker receive that volume, read-only. Stage pods do
not receive the wrapping key. Pod signing keys and API TLS keys stay separate.
The generator never generates a new key during rendering or manifest regeneration.

To rotate, provision a new immutable PEM version in the same Secret, retain every
old version, then change `active` and restart daemon/worker/codec-server pods to
refresh their private copies. Never replace material under an existing version.
Do not delete old versions while retained histories or backups reference them.

For an existing Key Vault or other explicitly configured codec, pass
`--temporal-codec-key-secret=` and supply that store's workload identity/mounts and
network access through your overlay. The generator refuses to overwrite an
existing codec configuration. An empty flag with no existing keyRef is an explicit
plaintext opt-out; use it only when that is the intended deployment policy.

The remote decoder is a separate `goobers temporal codec-server` process with the
same key store. It requires the instance's OIDC role mappings and explicit TLS
certificate/key flags. Expose it only through your configured HTTPS endpoint and
allow the exact Temporal Web UI origin; every decode still requires a bearer token
with `view`. See [the codec guide](../../../docs/guides/temporal-payload-codec.md).
The reference does not expose an anonymous decoder or modify Temporal's ingress.
