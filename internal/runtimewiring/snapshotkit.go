package runtimewiring

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// BuildAgenticKit binds captured instructions and assets to the snapshot directory.
func (s *ConfigSnapshot) BuildAgenticKit(l instance.Layout, env apiv1.InvocationEnvelope, mode agentickit.Mode, registrar *journal.RegistryScrubber) (*agentickit.Kit, error) {
	if s.ConfigDirectory != "" {
		l = l.WithConfigDir(s.ConfigDirectory)
	}
	return BuildAgenticKit(l, s.Config, s.Definitions, s.Instructions, s.SkillPackages, env, mode, registrar)
}
