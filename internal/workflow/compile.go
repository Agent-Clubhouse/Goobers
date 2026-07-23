package workflow

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/supportmatrix"
	vcurrent "github.com/goobers/goobers/internal/workflow/v_current"
)

// Option customizes compilation.
type Option = vcurrent.Option

// WithGoobers supplies goober definitions for capability admission.
func WithGoobers(goobers map[string]apiv1.GooberSpec) Option {
	return vcurrent.WithGoobers(goobers)
}

// WithKnownChecks supplies the registered automated-check names.
func WithKnownChecks(names []string) Option {
	return vcurrent.WithKnownChecks(names)
}

// WithKnownHarnesses supplies the registered agent harness names.
func WithKnownHarnesses(names []string) Option {
	return vcurrent.WithKnownHarnesses(names)
}

// PreviewFeaturesAnnotation enables preview DSL features on an instance.
const PreviewFeaturesAnnotation = vcurrent.PreviewFeaturesAnnotation

// PreviewFeaturesEnabled reports whether annotations explicitly enable previews.
func PreviewFeaturesEnabled(annotations map[string]string) bool {
	return vcurrent.PreviewFeaturesEnabled(annotations)
}

// WithPreviewFeatures applies preview-feature acknowledgement to compilation.
func WithPreviewFeatures(enabled bool) Option {
	return vcurrent.WithPreviewFeatures(enabled)
}

// Compile dispatches a pinned definition to its versioned interpreter.
func Compile(def Definition, opts ...Option) (*Machine, error) {
	version := def.DSLVersion
	if version == "" {
		version = supportmatrix.CurrentDSLVersion
	}

	support, ok := supportmatrix.GetDSL().Lookup(version)
	if !ok {
		return nil, fmt.Errorf("compile workflow %q: DSL version %q is not supported by this build", def.Name, version)
	}
	if support.Level == supportmatrix.LevelUnsupported {
		if support.Replacement != "" {
			return nil, fmt.Errorf("compile workflow %q: DSL version %q is unsupported; migrate to %q", def.Name, version, support.Replacement)
		}
		return nil, fmt.Errorf("compile workflow %q: DSL version %q is unsupported", def.Name, version)
	}

	switch version {
	case supportmatrix.CurrentDSLVersion:
		return vcurrent.Compile(def, opts...)
	default:
		return nil, fmt.Errorf("compile workflow %q: DSL version %q is declared %s but has no interpreter", def.Name, version, support.Level)
	}
}
