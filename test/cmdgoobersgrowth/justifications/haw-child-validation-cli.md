# cmd/goobers growth: child proposal validation

HAW-CHD-002 adds the author-facing `workflow validate-child` entry point. The
command package contains only the registry/help/completion declarations and a
small adapter to its existing harness registry and Goober admission functions.
Reusable configuration selection, grant derivation, bounded proposal reading,
validation, and report rendering live in `internal/childworkflow`.

The workflow command group moves into its own constructor so adding this entry
does not grow the large registry initializer. This command performs advisory
validation against the active configuration; it does not start or queue a run.
