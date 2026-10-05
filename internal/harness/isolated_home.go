package harness

import (
	"errors"
	"path/filepath"

	"github.com/goobers/goobers/internal/procenv"
)

// IsolatedHome is a trusted launcher-supplied empty runtime home. It is not a
// workflow option. Local interactive execution uses it to remove ambient CLI,
// Git and cloud identities before capability credentials are materialized.
func isolatedHomeEnvironment(home string) ([]string, error) {
	if !filepath.IsAbs(home) {
		return nil, errors.New("harness: isolated home must be absolute")
	}
	return procenv.IsolatedIdentityEnvironment(home), nil
}
