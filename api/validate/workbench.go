package validate

import "github.com/goobers/goobers/internal/workbench"

const errorWorkbenchSources WarningCode = "BKL001"

func (ix *index) checkWorkbenchSources(r *Report) {
	for _, name := range sortedGaggleNames(ix.gaggles) {
		if _, err := workbench.BindSources(ix.gaggles[name]); err != nil {
			r.add(errorWorkbenchSources, Error, ix.gaggleFile[name], "Gaggle", name, "spec.workbench: %v", err)
		}
	}
}
