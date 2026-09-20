package workflowsafety

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

// Annotation is the optional workflow annotation carrying safety assertions.
const Annotation = "goobers.dev/safety"

// Contracts are optional author assertions in a Workflow annotation. They do
// not grant permissions, affect execution, or certify custom code.
type Contracts struct {
	Version      int                      `json:"version"`
	Stages       map[string]StageContract `json:"stages,omitempty"`
	Suppressions []Suppression            `json:"suppressions,omitempty"`
}

// StageContract asserts only the named role or effects of an existing stage.
type StageContract struct {
	Review         string `json:"review,omitempty"`
	Evidence       string `json:"evidence,omitempty"`
	Publishes      string `json:"publishes,omitempty"`
	ChangesSubject bool   `json:"changesSubject,omitempty"`
	Parks          bool   `json:"parks,omitempty"`
	// RecoveryOnNoWork declares an obligation, not merely a reachable stage.
	RecoveryOnNoWork string `json:"recoveryOnNoWork,omitempty"`
}

// Suppression scopes a justified advisory waiver to a workflow or stage.
type Suppression struct {
	Code   string `json:"code"`
	Stage  string `json:"stage,omitempty"`
	Reason string `json:"reason"`
}

func parseContracts(m *wf.Machine) (Contracts, error) {
	var c Contracts
	raw := m.Def.Annotations[Annotation]
	if raw == "" {
		return c, nil
	}
	if len(raw) > 32768 {
		return c, fmt.Errorf("%s exceeds the 32768-byte analysis limit", Annotation)
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return Contracts{}, fmt.Errorf("%s: %w", Annotation, err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Contracts{}, fmt.Errorf("%s must contain exactly one JSON object", Annotation)
	}
	if c.Version != 1 {
		return Contracts{}, fmt.Errorf("%s version must be 1", Annotation)
	}
	for _, name := range sortedKeys(c.Stages) {
		if err := validateStageContract(m, name, c.Stages[name]); err != nil {
			return Contracts{}, err
		}
	}
	for _, s := range c.Suppressions {
		if !slices.Contains([]string{EvidenceCode, PublishCode, FeedbackCode, RecoveryCode, CycleCode, CoverageCode}, s.Code) ||
			strings.TrimSpace(s.Reason) == "" || (s.Stage != "" && !m.Has(s.Stage)) {
			return Contracts{}, fmt.Errorf("%s: suppression must name a safety rule, an existing stage (or whole workflow), and a nonempty reason", Annotation)
		}
	}
	return c, nil
}

func validateStageContract(m *wf.Machine, name string, s StageContract) error {
	if !m.Has(name) || !slices.Contains([]string{"", "code", "pr", "internal"}, s.Review) ||
		!slices.Contains([]string{"", "patch", "none"}, s.Evidence) {
		return fmt.Errorf("%s: invalid stage contract %q (review: code/pr/internal; evidence: patch/none)", Annotation, name)
	}
	if s.Review != "" {
		if g, ok := m.Gate(name); !ok || g.Agentic == nil {
			return fmt.Errorf("%s: review profile %q requires an agentic gate", Annotation, name)
		}
	}
	task, isTask := m.Task(name)
	if !isTask && (s.Evidence != "" || s.ChangesSubject || s.Parks || s.RecoveryOnNoWork != "") {
		return fmt.Errorf("%s: evidence, subject changes, parking and recovery assertions require a task, not %q", Annotation, name)
	}
	if !isTask && s.Publishes != "" && s.Publishes != name {
		return fmt.Errorf("%s: a gate can only assert publication of its own verdict (%q)", Annotation, name)
	}
	if s.Publishes != "" {
		if _, ok := m.Gate(s.Publishes); !ok {
			return fmt.Errorf("%s: publisher %q names unknown gate %q", Annotation, name, s.Publishes)
		}
	}
	if s.RecoveryOnNoWork != "" && !m.Has(s.RecoveryOnNoWork) {
		return fmt.Errorf("%s: recovery obligation at %q names unknown stage %q", Annotation, name, s.RecoveryOnNoWork)
	}
	if isTask {
		return validateKnownEffects(task, s)
	}
	return nil
}

func validateKnownEffects(task apiv1.Task, s StageContract) error {
	e := CommandEffects(task)
	if e.Known && ((s.Evidence == "patch" && !e.Patch) || (s.Evidence == "none" && e.Patch) ||
		(s.Publishes != "" && s.Publishes != e.Publishes) || (s.ChangesSubject && !e.Changes) || (s.Parks && !e.Parks)) {
		return fmt.Errorf("%s: assertion for %q contradicts a known command effect; use assertions for custom behavior", Annotation, task.Name)
	}
	return nil
}

func (c StageContract) assertsEffects() bool {
	return c.Evidence != "" || c.Publishes != "" || c.ChangesSubject || c.Parks
}

func (c StageContract) apply(e Effects) Effects {
	if c.Evidence == "patch" {
		e.Patch = true
	}
	if c.Evidence == "none" {
		e.EmptySuccess = true
	}
	if c.Publishes != "" {
		e.Publishes = c.Publishes
	}
	e.Changes = e.Changes || c.ChangesSubject
	e.Parks = e.Parks || c.Parks
	return e
}
