// Package goobers exposes release assets shared by Goobers binaries.
package goobers

import "embed"

// AgentToolkitAssets contains the canonical source material used to build the
// release-matched repository-side toolkit.
//
//go:embed agent-toolkit/.gitattributes agent-toolkit/README.md agent-toolkit/adapters agent-toolkit/instructions api/schemas/*.schema.json config-examples docs/ARCHITECTURE.md docs/requirements docs/stage-contract.md internal/capability/capability.go skills
var AgentToolkitAssets embed.FS
