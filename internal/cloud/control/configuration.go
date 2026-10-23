package control

import (
	"context"

	"github.com/goobers/goobers/internal/cloud/startfence"
)

// Configuration is the process-wide applier and its bound generation guard.
// Reload loads the candidate and calls that same applier's Apply method. The
// adapter must preserve the previous guard if a candidate is refused.
type Configuration interface {
	startfence.GenerationAdmission
	Reload(context.Context) error
}
