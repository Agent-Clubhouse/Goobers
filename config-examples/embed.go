// Package configexamples exposes the canonical example definitions to the
// guided instance initializer.
package configexamples

import "embed"

// Files contains the canonical workflows and their goober definitions.
//
//go:embed gaggles/acme-web/workflows/quickstart.yaml
//go:embed gaggles/acme-web/workflows/implementation.yaml
//go:embed gaggles/acme-web/workflows/backlog-curation.yaml
//go:embed gaggles/acme-web/workflows/work-nomination.yaml
//go:embed gaggles/acme-web/goobers/quickstart-implementer
//go:embed gaggles/acme-web/goobers/quickstart-reviewer
//go:embed gaggles/acme-web/goobers/implementer
//go:embed gaggles/acme-web/goobers/reviewer
//go:embed gaggles/acme-web/goobers/curator
//go:embed gaggles/acme-web/goobers/nominator
var Files embed.FS
