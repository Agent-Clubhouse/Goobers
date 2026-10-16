//go:build unix && !linux && !darwin

package proc

import "fmt"

func quiescencePIDs(int) ([]int, error) {
	return nil, fmt.Errorf("workspace writer inventory is unsupported")
}
