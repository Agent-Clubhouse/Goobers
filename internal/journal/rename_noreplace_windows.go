//go:build windows

package journal

import "github.com/goobers/goobers/internal/platform/durability"

func renameNoReplace(source, destination string) error {
	return durability.Move(source, destination)
}
