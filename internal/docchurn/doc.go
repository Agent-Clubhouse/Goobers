// Package docchurn builds and emits the docs-updater churn digest and advances
// its durable watermark.
//
// The package owns window calculation, Git response parsing, digest generation,
// output ordering, and watermark persistence. Callers own CLI parsing, input
// precedence, instance path resolution, and Git process construction, supplying
// the clock and Git operation explicitly to Run. Package tests use scripted Git
// operations; command tests retain CLI and real-Git coverage.
package docchurn
