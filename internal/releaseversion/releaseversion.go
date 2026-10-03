// Package releaseversion parses canonical release version strings.
package releaseversion

import (
	"fmt"
	"strconv"
	"strings"
)

// Options configures development-sentinel handling.
type Options struct {
	DevelopmentSentinel string
	AllowDevelopment    bool
}

// Version is a parsed canonical release version.
type Version struct {
	Development bool
	Major       uint64
	Minor       uint64
	Patch       uint64
}

// Parse parses a canonical vMAJOR.MINOR.PATCH release version or an allowed
// development sentinel.
func Parse(value string, opts Options) (Version, error) {
	if opts.DevelopmentSentinel != "" && value == opts.DevelopmentSentinel {
		if opts.AllowDevelopment {
			return Version{Development: true}, nil
		}
		return Version{}, fmt.Errorf("%q is only valid for the initial pre-release baseline", value)
	}
	if value != strings.TrimSpace(value) || !strings.HasPrefix(value, "v") {
		return Version{}, fmt.Errorf("must use vMAJOR.MINOR.PATCH")
	}
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("must use vMAJOR.MINOR.PATCH")
	}
	numbers := make([]uint64, len(parts))
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return Version{}, fmt.Errorf("must use canonical vMAJOR.MINOR.PATCH")
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("must use vMAJOR.MINOR.PATCH")
		}
		numbers[i] = number
	}
	return Version{Major: numbers[0], Minor: numbers[1], Patch: numbers[2]}, nil
}
