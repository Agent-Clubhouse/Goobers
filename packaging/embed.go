// Package packaging embeds platform assets shipped with the Goobers binary.
package packaging

import "embed"

// ServiceFiles contains the platform supervisor definitions shipped with the
// goobers binary.
//
//go:embed systemd/goobers.service launchd/com.agent-clubhouse.goobers.plist
var ServiceFiles embed.FS
