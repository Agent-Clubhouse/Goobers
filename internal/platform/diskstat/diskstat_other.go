//go:build !linux && !darwin && !windows

package diskstat

import "fmt"

func read(path string) (Footprint, error) {
	return Footprint{}, fmt.Errorf("diskstat: free-space measurement unsupported on this platform")
}
