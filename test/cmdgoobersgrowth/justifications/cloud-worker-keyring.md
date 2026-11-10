# Explicit cloud worker keyring inputs

`cmd/goobers-cloud` adds CLI wiring for `--config-keyring-file` and selects
`LoadKeyRingFile` for that flag and `api.podTokenKeyRingFile`. The command rejects
conflicting raw/ring inputs, documents the flag, and retains the exact existing
raw config-key loading path. These are worker composition-root decisions; ring
parsing, validation, versioned token minting and verification remain in
`internal/podauth`. No new production command file is added.

The explicit selection is necessary to avoid reinterpreting arbitrary legacy
raw bytes, including brace-prefixed keys and JSON, as a key ring.
