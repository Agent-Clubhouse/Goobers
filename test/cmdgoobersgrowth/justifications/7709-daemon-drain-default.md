# #7709: Preserve the local daemon drain default

Growth: +5 non-test lines in `cmd/goobers`.

The command package names the existing zero-duration local drain default and
documents why it must remain unbounded: local modes execute long agentic stages
inside the daemon. The reference remote-dispatch deployment opts into its
60-second bound through explicit CLI arguments instead of changing local
behavior.

Keeping this constant beside the `up` flag registration makes the distinction
executable and lets the regression test reject a future global bounded default.
