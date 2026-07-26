//go:build !windows

package executor

func scriptCommand(script string) ([]string, []string, func(), error) {
	return []string{"sh", "-c", script}, nil, func() {}, nil
}
