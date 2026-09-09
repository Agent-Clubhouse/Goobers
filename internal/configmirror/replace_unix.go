//go:build !windows

package configmirror

import "github.com/goobers/goobers/internal/platform/durability"

func replaceSnapshot(source, destination string) error {
	return durability.ReplaceFile(source, destination)
}
