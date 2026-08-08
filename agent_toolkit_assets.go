// Package goobers exposes release assets shared by Goobers binaries.
package goobers

import "embed"

// AgentToolkitAssets contains the canonical source material used to build the
// release-matched repository-side toolkit.
//
//go:embed agent-toolkit/.gitattributes agent-toolkit/README.md agent-toolkit/adapters agent-toolkit/instructions api/schemas/*.schema.json config-examples docs/ARCHITECTURE.md docs/VISION.md docs/adr/0001-agentic-sandbox-mechanism.md docs/adr/0002-provider-neutral-capability-namespaces.md docs/design/cobrand.md docs/design/notification-output.md docs/design/static-fan-out-fan-in.md docs/guides/goobers-io-mcp.md docs/guides/quickstart.md docs/guides/supervision.md docs/requirements docs/stage-contract.md internal/capability/capability.go skills
var AgentToolkitAssets embed.FS
