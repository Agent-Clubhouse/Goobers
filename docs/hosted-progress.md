# GitHub-hosted live progress

GitHub Actions-hosted Goobers runs can publish a bounded live projection for
the Goobers Portal while the final journal artifact is not yet available. The
Goobers CLI owns the `goobers.dev/hosted-progress/v1` contract and publishes it
to one Check Run; hosted workflows do not need to implement journal parsing.

```yaml
permissions:
  contents: read
  checks: write

steps:
  - uses: actions/checkout@v4
  - name: Run Goobers
    env:
      GITHUB_TOKEN: ${{ github.token }}
    run: goobers run --github-progress implement-locally .
  - name: Upload final Goobers journal
    if: always()
    uses: actions/upload-artifact@v4
    with:
      name: goobers-journal
      path: gaggles/*/runs/**
```

The Check Run is named `Goobers / <workflow>`. Its `external_id` is:

```text
goobers.dev/hosted-progress/v1:<goobers-run-id>:<actions-run-id>
```

The Check Run output embeds JSON conforming to
[`api/schemas/hosted-progress.schema.json`](../api/schemas/hosted-progress.schema.json)
between these markers:

```text
<!-- goobers-progress:v1 -->
<!-- /goobers-progress:v1 -->
```

The payload contains the Actions identity and URL, canonical immutable
`RunIdentity`, current phase, pinned workflow graph, transition and
operator-relevant journal events, revision, and update time. Updates occur only
when the committed journal sequence advances. Older projected events may be
removed to remain within the Check Run output limit; `truncatedBefore` records
that boundary.

Consumers must match both `schema` and `actionsRunId`. They should use the
highest matching revision and replace the projection with the final journal
artifact as soon as it is available.
