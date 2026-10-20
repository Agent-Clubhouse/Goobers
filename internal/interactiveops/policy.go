package interactiveops

import (
	"errors"
	"fmt"
	"sort"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Policy is the immutable host-derived session authority snapshot. An explicit
// empty collection grants nothing. It is never accepted from browser JSON.
type Policy struct {
	Revision            string              `json:"revision"`
	Gaggle              string              `json:"gaggle"`
	Workflows           []string            `json:"workflows"`
	CommandCapabilities []CommandCapability `json:"commandCapabilities"`
	AllowAuthoring      bool                `json:"allowAuthoring"`
}

func (s *Service) policyFor(scope Scope) (Policy, error) {
	if s.opts.PolicyForScope == nil {
		return Policy{Gaggle: "interactive", CommandCapabilities: append([]CommandCapability{}, s.opts.CommandCapabilities...), AllowAuthoring: true}, nil
	}
	p, err := s.opts.PolicyForScope(scope)
	if err != nil {
		return Policy{}, err
	}
	if !namePattern.MatchString(p.Gaggle) {
		return Policy{}, errors.New("session policy does not authorize a gaggle")
	}
	p.Workflows = append([]string{}, p.Workflows...)
	p.CommandCapabilities = append([]CommandCapability{}, p.CommandCapabilities...)
	seen := map[string]bool{}
	for _, name := range p.Workflows {
		if !namePattern.MatchString(name) || seen[name] {
			return Policy{}, errors.New("invalid or duplicate policy workflow")
		}
		seen[name] = true
	}
	caps := map[CommandCapability]bool{}
	for _, c := range p.CommandCapabilities {
		if (c != CommandNodeTest && c != CommandNodeCheck) || caps[c] {
			return Policy{}, errors.New("unknown or duplicate scoped command capability")
		}
		if !s.allows(c) {
			return Policy{}, errors.New("scoped command capability exceeds host authority")
		}
		caps[c] = true
	}
	sort.Strings(p.Workflows)
	sort.Slice(p.CommandCapabilities, func(i, j int) bool { return p.CommandCapabilities[i] < p.CommandCapabilities[j] })
	return p, nil
}
func (s *Service) scoped(scope Scope) (*Service, Policy, error) {
	p, err := s.policyFor(scope)
	if err != nil {
		return nil, p, err
	}
	view := *s
	view.policy = &p
	view.opts.CommandCapabilities = p.CommandCapabilities
	return &view, p, nil
}
func (s *Service) policyGaggle() string {
	if s.policy != nil {
		return s.policy.Gaggle
	}
	return "interactive"
}
func (s *Service) checkWorkflowAuthority(dir string, p Policy, name string) error {
	if s.opts.PolicyForScope == nil {
		return nil
	}
	for _, allowed := range p.Workflows {
		if name == allowed {
			return nil
		}
	}
	// An immutable, successfully committed authoring receipt is the ownership
	// registry. An interrupted transaction is reconciled before use, and no
	// unrelated on-disk workflow can acquire authority merely by appearing.
	if p.AllowAuthoring {
		actions, err := allActions(dir)
		if err != nil {
			return err
		}
		for i := range actions {
			a := &actions[i]
			if a.Kind != "add_workflow" {
				continue
			}
			if isConfigAction(a.Kind) && a.Status != "succeeded" {
				s.reconcile(dir, a, nil)
			}
			if a.Status == "succeeded" && a.OperationStatus == "confirmed" {
				varName := workflowName(a.Request.WorkflowYAML)
				if varName == name && a.Request.Gaggle == p.Gaggle {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("workflow %q is outside the immutable session allowlist", name)
}
func workflowName(content string) string {
	var wf apiv1.Workflow
	if yaml.UnmarshalStrict([]byte(content), &wf) != nil {
		return ""
	}
	return wf.Name
}
func (s *Service) configDir(scope Scope) (string, error) {
	path := s.opts.ConfigDir
	if s.opts.ConfigDirForScope != nil {
		var err error
		path, err = s.opts.ConfigDirForScope(scope)
		if err != nil {
			return "", err
		}
		if path == "" {
			return "", errors.New("session config source is unavailable")
		}
	}
	if path == "" {
		return "", nil
	}
	return canonicalRepository(path)
}
