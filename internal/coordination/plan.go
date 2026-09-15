// Package coordination reconciles explicitly approved, repository-qualified
// work plans. It never implements code, merges pull requests, or deploys.
package coordination

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/goobers/goobers/providers"
)

// Repository is an explicit GitHub repository identity, not an additionalRepo.
type Repository struct {
	Provider string `json:"provider" yaml:"provider"`
	Owner    string `json:"owner" yaml:"owner"`
	Name     string `json:"name" yaml:"name"`
	BaseURL  string `json:"baseUrl,omitempty" yaml:"baseUrl,omitempty"`
}

func (r Repository) Ref() providers.RepositoryRef {
	return providers.RepositoryRef{Provider: providers.ProviderKind(r.Provider), Owner: r.Owner, Name: r.Name, URL: r.BaseURL}
}

func (r Repository) Key() string { return r.Ref().CanonicalKey() }

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var numberPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

func (r Repository) Validate() error {
	if r.Provider != "github" || !namePattern.MatchString(r.Owner) || !namePattern.MatchString(r.Name) {
		return fmt.Errorf("coordination requires an explicit GitHub provider/owner/name")
	}
	// Initial local delivery intentionally excludes enterprise endpoint/token
	// ambiguity. The wire field is reserved, but unsupported values fail closed.
	if r.BaseURL != "" {
		return fmt.Errorf("coordination supports github.com with omitted baseUrl only")
	}
	return nil
}

type Target struct {
	Repository Repository `json:"repository" yaml:"repository"`
	Approval   string     `json:"approval" yaml:"approval"`
}

// Authority lives in trusted instance configuration, never in a work plan.
type Authority struct {
	Name             string            `json:"name" yaml:"name"`
	ParentRepo       Repository        `json:"parentRepo" yaml:"parentRepo"`
	Targets          []Target          `json:"targets" yaml:"targets"`
	ApprovedPlans    map[string]string `json:"approvedPlans,omitempty" yaml:"approvedPlans,omitempty"`
	ApprovedEvidence map[string]string `json:"approvedEvidence,omitempty" yaml:"approvedEvidence,omitempty"`
}

type Configuration struct {
	Gaggles []Authority `json:"gaggles" yaml:"gaggles"`
}

func (a Authority) Validate() error {
	if !namePattern.MatchString(a.Name) {
		return fmt.Errorf("coordination gaggle name is required")
	}
	if err := a.ParentRepo.Validate(); err != nil {
		return err
	}
	if len(a.Targets) == 0 {
		return fmt.Errorf("coordination requires explicit targets")
	}
	seen := map[string]bool{}
	for _, target := range a.Targets {
		if err := target.Repository.Validate(); err != nil {
			return err
		}
		if target.Approval != "reviewed-plan" {
			return fmt.Errorf("target approval must be reviewed-plan")
		}
		if seen[target.Repository.Key()] {
			return fmt.Errorf("duplicate coordination target")
		}
		seen[target.Repository.Key()] = true
	}
	for _, approvals := range []map[string]string{a.ApprovedPlans, a.ApprovedEvidence} {
		for id, digest := range approvals {
			if !namePattern.MatchString(id) || !digestPattern.MatchString(digest) {
				return fmt.Errorf("coordination approvals require a plan id and lowercase SHA-256 digest")
			}
		}
	}
	return nil
}

type NodeRef struct {
	Repository Repository `json:"repository"`
	ID         string     `json:"id"`
}

func (n NodeRef) Key() string { return n.Repository.Key() + "#" + n.ID }

type Child struct {
	NodeRef
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Kind       string    `json:"kind"`
	DependsOn  []NodeRef `json:"dependsOn,omitempty"`
	Completion string    `json:"completion"`
	ReleaseTag string    `json:"releaseTag,omitempty"`
}

type Plan struct {
	Version int     `json:"version"`
	ID      string  `json:"id"`
	Gaggle  string  `json:"gaggle"`
	Parent  NodeRef `json:"parent"`
	// Summary is reviewed publication text for the parent tracking section.
	Summary            string  `json:"summary"`
	Children           []Child `json:"children"`
	IntegrationCommand string  `json:"integrationCommand"`
}

func Digest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Decode refuses unknown fields and trailing documents, including misspelled
// approval/evidence properties that would otherwise look accepted.
func Decode(r io.Reader, value any) error {
	d := json.NewDecoder(io.LimitReader(r, 2<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON document: %v", err)
	}
	return nil
}

func (p Plan) Validate(a Authority, requireApproval bool) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if p.Version != 1 || !namePattern.MatchString(p.ID) || p.Gaggle != a.Name {
		return fmt.Errorf("plan version, id, or owning gaggle is invalid")
	}
	if err := p.Parent.Repository.Validate(); err != nil {
		return err
	}
	if p.Parent.Repository.Key() != a.ParentRepo.Key() || !numberPattern.MatchString(p.Parent.ID) {
		return fmt.Errorf("parent does not match coordinator authority")
	}
	if strings.TrimSpace(p.Summary) == "" || strings.TrimSpace(p.IntegrationCommand) == "" || len(p.Children) == 0 || len(p.Children) > 100 {
		return fmt.Errorf("plan requires reviewed summary, integration command, and 1-100 children")
	}
	if strings.Contains(p.Summary, "goobers-coordination:") {
		return fmt.Errorf("summary contains reserved coordination marker")
	}
	allowed := map[string]bool{}
	for _, target := range a.Targets {
		allowed[target.Repository.Key()] = true
	}
	children := map[string]Child{}
	for _, child := range p.Children {
		if err := child.Repository.Validate(); err != nil {
			return err
		}
		if !allowed[child.Repository.Key()] {
			return fmt.Errorf("unauthorized target for child %s", child.ID)
		}
		if !namePattern.MatchString(child.ID) || strings.TrimSpace(child.Title) == "" || strings.TrimSpace(child.Body) == "" {
			return fmt.Errorf("child requires id and explicitly reviewed title/body")
		}
		if strings.Contains(child.Body, "goobers-coordination:") || strings.Contains(child.Title, "goobers-coordination:") {
			return fmt.Errorf("child content contains reserved coordination marker")
		}
		if child.Kind != "implementation" && child.Kind != "task" {
			return fmt.Errorf("invalid child kind")
		}
		switch child.Completion {
		case "merged":
		case "release":
			if child.ReleaseTag == "" {
				return fmt.Errorf("release completion requires exact releaseTag")
			}
		case "closed":
			if child.Kind != "task" {
				return fmt.Errorf("issue closure cannot prove implementation")
			}
		default:
			return fmt.Errorf("completion must be merged, release, or closed")
		}
		if child.Completion != "release" && child.ReleaseTag != "" {
			return fmt.Errorf("releaseTag requires release completion")
		}
		if _, exists := children[child.Key()]; exists {
			return fmt.Errorf("duplicate child %s", child.Key())
		}
		children[child.Key()] = child
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(key string) error {
		if visiting[key] {
			return fmt.Errorf("dependency cycle at %s", key)
		}
		if visited[key] {
			return nil
		}
		child, exists := children[key]
		if !exists {
			return fmt.Errorf("unknown repository-qualified dependency %s", key)
		}
		visiting[key] = true
		for _, dep := range child.DependsOn {
			if err := dep.Repository.Validate(); err != nil {
				return err
			}
			if !namePattern.MatchString(dep.ID) {
				return fmt.Errorf("invalid dependency id")
			}
			if err := visit(dep.Key()); err != nil {
				return err
			}
		}
		visiting[key], visited[key] = false, true
		return nil
	}
	for key := range children {
		if err := visit(key); err != nil {
			return err
		}
	}
	if requireApproval {
		digest, err := Digest(p)
		if err != nil {
			return err
		}
		if a.ApprovedPlans[p.ID] != digest {
			return fmt.Errorf("plan content is not approved in trusted instance configuration")
		}
	}
	return nil
}
