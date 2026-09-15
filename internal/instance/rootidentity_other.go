//go:build !windows

package instance

func rootIdentityReadContended(error) bool {
	return false
}
