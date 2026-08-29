//go:build unix && !linux

package secfile

func isReadOnlyTmpfs(string) bool {
	return false
}
