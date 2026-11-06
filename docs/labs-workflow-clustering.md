# Labs workflow clustering

`goobers workflow clusters` is experimental, advisory analysis. Structural
feature vectors use `workflowcluster.structural/v2`.

## Migration from structural/v1

Version 2 removes `goal.chars`, timeout duration totals, and
`retry.attempts`, which measured writing and configuration style more than
workflow shape. The presence of timeouts and retries remains represented. It
widens the hashed Weisfeiler-Lehman histogram from 32 to 128 buckets while
retaining the established per-bucket down-weight. Persisted v1 vectors and
models are intentionally incompatible with v2; regenerate snapshots and refit
models instead of mixing versions.

Use `--ablation` to report mean cluster stability and assignment change after
dropping each feature. The WL histogram is ablated as one group. A feature is
flagged dominant when either change is at least 0.20.

Fitting is exact for populations of at most 2,000 points. Above that boundary,
the command runs HDBSCAN on a deterministic, evenly spaced 1,000-point sample
and assigns remaining points to a sampled cluster when they fall within its
radius. This bounds the quadratic distance matrix; results above the boundary
are approximate.
