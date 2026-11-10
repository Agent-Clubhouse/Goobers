# Control object store: explicit opt-in shared-key credential

`goobers-cloud control` gains `--object-store-account-name` and
`--object-store-account-key-file`. When both are set, `bindControlObjectStore`
authenticates to Blob storage with a mounted storage-account key instead of the
default Azure credential chain. The nightly kind job needs this because it runs
against a local storage emulator, which has no token credential. Without the flags
the role keeps its default-credential behaviour.

The growth is flag parsing and choosing between two existing `objstore`
constructors. It reads command-layer flags and mounted files at boot, so it
belongs in cmd/goobers-cloud. It adds no command or lifecycle.
