package instance

import (
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/speechnotify"
	"github.com/goobers/goobers/internal/workcopyroot"
)

// PartialCloneEnabled reports whether newly created mirrors should be
// blobless partial clones (workcopies.partialClone, defaults to false).
func (c *Config) PartialCloneEnabled() bool {
	return c != nil && c.Workcopies != nil && c.Workcopies.PartialClone
}

// ObjectCacheEnabled reports whether newly created mirrors should reference
// a shared node-level object cache (workcopies.objectCache, defaults to
// false).
func (c *Config) ObjectCacheEnabled() bool {
	return c != nil && c.Workcopies != nil && c.Workcopies.ObjectCache
}

// EffectiveWorkcopiesLayout applies the gaggle override, then the instance
// override, to layout. An empty root preserves the instance-local default.
func EffectiveWorkcopiesLayout(layout Layout, c *Config, gaggle *apiv1.Gaggle) (Layout, error) {
	root, err := effectiveWorkcopiesRoot(c, gaggle)
	if err != nil || root == "" {
		if err != nil {
			return Layout{}, err
		}
		return layout, nil
	}
	return layout.WithWorkcopiesRoot(filepath.Clean(root)), nil
}

func effectiveWorkcopiesRoot(c *Config, gaggle *apiv1.Gaggle) (string, error) {
	root := ""
	if c != nil && c.Workcopies != nil {
		root = c.Workcopies.Root
	}
	if gaggle != nil && gaggle.Spec.Workcopies != nil && gaggle.Spec.Workcopies.Root != "" {
		root = gaggle.Spec.Workcopies.Root
	}
	if root == "" {
		return "", nil
	}
	if err := workcopyroot.Validate("workcopies.root", root); err != nil {
		return "", err
	}
	return root, nil
}

// EffectiveSelfIdentity returns the provider login configured for gaggle,
// falling back to the instance-wide default. Empty means assignment-aware
// backlog selection remains opted out.
func EffectiveSelfIdentity(c *Config, gaggle *apiv1.Gaggle) string {
	if gaggle != nil && gaggle.Spec.SelfIdentity != "" {
		return gaggle.Spec.SelfIdentity
	}
	if c == nil {
		return ""
	}
	return c.SelfIdentity
}

// EffectiveSpeechConfig returns the configured speech settings or disabled
// defaults when the speech section is absent.
func (c *Config) EffectiveSpeechConfig() speechnotify.Config {
	if c == nil || c.Speech == nil {
		return speechnotify.Config{}
	}
	return *c.Speech
}
