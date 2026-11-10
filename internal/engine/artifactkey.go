package engine

import "fmt"

func projectedArtifactKey(branch int, name string) string {
	return fmt.Sprintf("%d:%s", branch, name)
}
