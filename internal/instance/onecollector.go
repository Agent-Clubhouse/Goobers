package instance

import (
	"errors"
	"path/filepath"
)

// OneCollectorConfig references a separately provisioned protected profile.
// Its environment opt-in is independent of the public telemetry setting.
type OneCollectorConfig struct {
	ProfileFile string `json:"profile_file" yaml:"profile_file"`
}

// Validate requires an absolute path for a configured protected profile.
func (c *OneCollectorConfig) Validate() error {
	if c != nil && !filepath.IsAbs(c.ProfileFile) {
		return errors.New("telemetry.onecollector.profile_file must be an absolute path")
	}
	return nil
}
