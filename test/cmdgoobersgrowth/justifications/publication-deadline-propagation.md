# Preserve publication inspection deadlines

The command package grows by six non-test Go lines and zero files to preserve
the caller's cancellation or deadline when local Git inspection fails, and to
stop base-reference fallback once that budget expires. These checks belong in
the existing CLI Git adapter and base-resolution loop; the shared history-depth
error handling remains in `internal/publicationguard`. No publication bypass or
new command behavior is introduced.
