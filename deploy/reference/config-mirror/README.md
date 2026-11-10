# Rendered configuration mirror (compatibility alias)

The base worker now fetches sanitized configuration by content digest from the
daemon config plane, retains three superseded trees, stores its work root on
emptyDir, and uses the HTTP blob plane. It no longer needs the former RWX
configuration-mirror PVC or boot-time seed init container.

This overlay remains as a render-compatible alias for adopters that referenced
`deploy/reference/config-mirror`. Remove any downstream `configMirrorPath`,
mirror PVC, and `config-seed` patches. Configure the base worker's TLS trust and
the `goobers-pod-token-key` Secret instead.
