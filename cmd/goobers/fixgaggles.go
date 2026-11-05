package main

import (
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/dslmigrate"
	"github.com/goobers/goobers/internal/instance"
)

type gaggleFix struct {
	name, source string
	result       *dslmigrate.Result
}

// Preflight shared requirements before changing workflows. A refused gaggle
// must not leave its workflows upgraded against a legacy placement floor.
func prepareGaggleFixes(configDir string, set *instance.ConfigSet, stdout io.Writer) ([]gaggleFix, map[string]bool) {
	used := map[string]bool{}
	for _, wf := range set.Workflows {
		used[wf.Spec.Gaggle] = true
	}
	blocked := map[string]bool{}
	var fixes []gaggleFix
	for _, gaggle := range set.Gaggles {
		if !used[gaggle.Name] {
			continue
		}
		source, found := set.GaggleSource(gaggle.Name)
		if !found {
			continue
		}
		before, err := os.ReadFile(filepath.Join(configDir, source))
		var result *dslmigrate.Result
		if err == nil {
			result, err = dslmigrate.MigrateGaggleToV3(before)
		}
		if err != nil {
			pf(stdout, "FIX Gaggle/%s (%s): %v\n", gaggle.Name, source, err)
			blocked[gaggle.Name] = true
			continue
		}
		if result.Changed {
			fixes = append(fixes, gaggleFix{name: gaggle.Name, source: source, result: result})
		}
	}
	return fixes, blocked
}
