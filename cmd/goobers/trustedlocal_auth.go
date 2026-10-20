package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/harness"
)

// trustedLocalAuthenticationBinding is host-owned authority retained for the
// parent occurrence. V1 permits exactly one model ref or one selected login.
// Future DSL allowedAgentProfiles must explicitly extend this binding and its
// instance grant checks; another child's profile is never authority by itself.
type trustedLocalAuthenticationBinding struct {
	Harness  apiv1.Harness                           `json:"harness"`
	ModelRef string                                  `json:"modelRef,omitempty"`
	Ambient  *credentials.TrustedLocalAuthentication `json:"ambient,omitempty"`
}

func trustedLocalAuthenticationFor(goober string, spec apiv1.GooberSpec, grants []credentials.Grant) (trustedLocalAuthenticationBinding, error) {
	selected := spec.Harness
	if selected == "" {
		selected = apiv1.HarnessCopilot
	}
	binding := trustedLocalAuthenticationBinding{Harness: selected}
	switch selected {
	case apiv1.HarnessCopilot, apiv1.HarnessClaudeCode, apiv1.HarnessCodex:
	default:
		return binding, credentials.ErrChildAuthenticationIsolation
	}
	ambientCodex := selected == apiv1.HarnessCodex && harness.CodexUsesAmbientChatGPT(spec.HarnessOptions)
	if !ambientCodex {
		for _, grant := range buildGooberCredentialGrants(goober, string(selected), []string{"agent:model"}, grants) {
			if grant.Capability == "agent:model" {
				binding.ModelRef = grant.Ref
			}
		}
	}
	if binding.ModelRef != "" {
		return binding, nil
	}
	if selected == apiv1.HarnessCodex && !ambientCodex {
		return binding, credentials.ErrChildAuthenticationIsolation
	}
	var variable, directory string
	switch selected {
	case apiv1.HarnessCopilot:
		variable, directory = "COPILOT_HOME", ".copilot"
	case apiv1.HarnessClaudeCode:
		variable, directory = "CLAUDE_CONFIG_DIR", ".claude"
	case apiv1.HarnessCodex:
		variable, directory = "CODEX_HOME", ".codex"
	}
	home := os.Getenv(variable)
	if home == "" {
		root, err := os.UserHomeDir()
		if err != nil {
			return binding, err
		}
		home = filepath.Join(root, directory)
	}
	if !filepath.IsAbs(home) {
		return binding, errors.New("selected parent login home must be absolute")
	}
	auth := credentials.TrustedLocalAuthentication{Harness: string(selected), Home: home}
	if selected == apiv1.HarnessCodex {
		if option, ok := spec.HarnessOptions["allowFileBackedCredentials"]; ok {
			if err := json.Unmarshal(option.Raw, &auth.AllowFileBacked); err != nil {
				return binding, err
			}
		}
	}
	binding.Ambient = &auth
	return binding, nil
}

func (binding trustedLocalAuthenticationBinding) admitChild(goober string, spec apiv1.GooberSpec, grants []credentials.Grant) error {
	child, err := trustedLocalAuthenticationFor(goober, spec, grants)
	if err != nil {
		return err
	}
	if binding.ModelRef != "" {
		if child.ModelRef != binding.ModelRef || child.Ambient != nil {
			return credentials.ErrChildAuthenticationIsolation
		}
		return nil
	}
	if binding.Ambient == nil || child.Ambient == nil || child.ModelRef != "" || child.Harness != binding.Harness {
		return credentials.ErrChildAuthenticationIsolation
	}
	// A child may narrow a file-backed parent to keyring-only, never widen it.
	expected, actual := *binding.Ambient, *child.Ambient
	if actual.AllowFileBacked && !expected.AllowFileBacked {
		return credentials.ErrChildAuthenticationIsolation
	}
	expected.AllowFileBacked = actual.AllowFileBacked
	if !reflect.DeepEqual(expected, actual) {
		return credentials.ErrChildAuthenticationIsolation
	}
	return nil
}
