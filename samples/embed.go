// Package samples exposes versioned onboarding samples embedded in the binary.
package samples

import "embed"

// Files contains the complete disposable tutorial targets, including dotfiles.
//
//go:embed all:getting-started-task-api
var Files embed.FS
