package main

import (
	"fmt"
	"strings"

	"github.com/goobers/goobers/internal/readservice"
)

func engineFallbackStatusLines(status readservice.SchedulerStatus) string {
	var output strings.Builder
	for _, fallback := range status.EngineFallbacks {
		level := "INFO"
		if fallback.PlacementDeclared {
			level = "WARNING"
		}
		fmt.Fprintf(&output, "%s engine dispatch %s/%s: runner fallback [%s] (observed run %s): %s\n",
			level, fallback.Gaggle, fallback.Workflow, fallback.ReasonClass, fallback.RunID, fallback.Reason)
	}
	return output.String()
}
