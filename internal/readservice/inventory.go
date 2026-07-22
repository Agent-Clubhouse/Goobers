package readservice

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

const (
	// DefaultPageSize is used when a list request omits a limit.
	DefaultPageSize = 50
	// MaxPageSize bounds one inventory list response.
	MaxPageSize = 100

	currentWorkflowVersion = 1
)

var (
	// ErrInvalidCursor means a cursor is malformed or belongs to another list.
	ErrInvalidCursor = errors.New("invalid page cursor")
	// ErrInvalidPage means a page limit is outside the supported contract.
	ErrInvalidPage = errors.New("invalid page request")
)

// PageRequest selects one deterministic page from an inventory list.
type PageRequest struct {
	Limit  int
	Cursor string
}

// PageInfo describes a list page and the cursor for the next page.
type PageInfo struct {
	Limit      int    `json:"limit"`
	Total      int    `json:"total"`
	HasMore    bool   `json:"hasMore"`
	NextCursor string `json:"nextCursor"`
}

// InstanceStatus is the inventory-level state of the daemon and its config.
type InstanceStatus string

const (
	// InstanceStatusStarting means the daemon has not reached readiness.
	InstanceStatusStarting InstanceStatus = "starting"
	// InstanceStatusReady means the daemon is ready with no config warnings.
	InstanceStatusReady InstanceStatus = "ready"
	// InstanceStatusDegraded means the daemon is serving with config warnings.
	InstanceStatusDegraded InstanceStatus = "degraded"
)

// Instance is the overview inventory projection.
type Instance struct {
	APIVersion    string                  `json:"apiVersion"`
	SchemaVersion string                  `json:"schemaVersion"`
	Name          string                  `json:"name"`
	Environment   apiv1.Environment       `json:"environment"`
	Ready         bool                    `json:"ready"`
	Status        InstanceStatus          `json:"status"`
	Concurrency   Concurrency             `json:"concurrency"`
	Counts        InventoryCounts         `json:"counts"`
	Warnings      []validate.CodedWarning `json:"warnings"`
}

// InventoryCounts summarizes configured definitions and active runs.
type InventoryCounts struct {
	Gaggles    int `json:"gaggles"`
	Goobers    int `json:"goobers"`
	Workflows  int `json:"workflows"`
	ActiveRuns int `json:"activeRuns"`
}

// Concurrency reports current active runs against the configured maximum.
type Concurrency struct {
	ActiveRuns        int `json:"activeRuns"`
	MaxConcurrentRuns int `json:"maxConcurrentRuns"`
}

// DefinitionStatus describes whether a configured definition is available.
type DefinitionStatus string

const (
	// DefinitionStatusConfigured means the definition loaded successfully.
	DefinitionStatusConfigured DefinitionStatus = "configured"
)

// Gaggle is one configured workforce inventory item.
type Gaggle struct {
	Name           string                  `json:"name"`
	DisplayName    string                  `json:"displayName"`
	Status         DefinitionStatus        `json:"status"`
	Project        apiv1.RepoRef           `json:"project"`
	Backlog        apiv1.BacklogRef        `json:"backlog"`
	GooberCount    int                     `json:"gooberCount"`
	WorkflowCount  int                     `json:"workflowCount"`
	ActiveRunCount int                     `json:"activeRunCount"`
	Warnings       []validate.CodedWarning `json:"warnings"`
}

// GagglePage is a deterministic page of gaggles.
type GagglePage struct {
	Items []Gaggle `json:"items"`
	Page  PageInfo `json:"page"`
}

// WorkflowReference identifies a workflow within its gaggle.
type WorkflowReference struct {
	Gaggle string `json:"gaggle"`
	Name   string `json:"name"`
}

// GooberReference identifies a goober within its gaggle.
type GooberReference struct {
	Gaggle string `json:"gaggle"`
	Name   string `json:"name"`
}

// StageOwnership identifies a workflow stage owned by a goober.
type StageOwnership struct {
	Workflow WorkflowReference      `json:"workflow"`
	Stage    string                 `json:"stage"`
	Kind     workflow.GraphNodeKind `json:"kind"`
}

// Goober is one provisioned definition without inferred runtime health or
// capacity.
type Goober struct {
	Name         string                  `json:"name"`
	DisplayName  string                  `json:"displayName"`
	Role         string                  `json:"role"`
	Status       DefinitionStatus        `json:"status"`
	Harness      apiv1.Harness           `json:"harness"`
	Skills       []string                `json:"skills"`
	Capabilities []string                `json:"capabilities"`
	Workflows    []WorkflowReference     `json:"workflows"`
	Stages       []StageOwnership        `json:"stages"`
	Warnings     []validate.CodedWarning `json:"warnings"`
}

// GooberPage is a deterministic page of goobers.
type GooberPage struct {
	Items []Goober `json:"items"`
	Page  PageInfo `json:"page"`
}

// WorkflowDefinition identifies the current compiled workflow definition.
type WorkflowDefinition struct {
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

// WorkflowConcurrency reports active and maximum concurrent runs.
type WorkflowConcurrency struct {
	ActiveRuns        int32 `json:"activeRuns"`
	MaxConcurrentRuns int32 `json:"maxConcurrentRuns"`
}

// WorkflowSummary is the inventory projection shared by workflow list and
// detail responses.
type WorkflowSummary struct {
	Identity    WorkflowReference         `json:"identity"`
	DisplayName string                    `json:"displayName"`
	Purpose     string                    `json:"purpose"`
	Triggers    []apiv1.Trigger           `json:"triggers"`
	Readiness   apiv1.ReadinessConditions `json:"readiness"`
	Concurrency WorkflowConcurrency       `json:"concurrency"`
	Owners      []GooberReference         `json:"owners"`
	StageCount  int                       `json:"stageCount"`
	Definition  WorkflowDefinition        `json:"definition"`
	Warnings    []validate.CodedWarning   `json:"warnings"`
}

// WorkflowPage is a deterministic page of workflows within one gaggle.
type WorkflowPage struct {
	Items []WorkflowSummary `json:"items"`
	Page  PageInfo          `json:"page"`
}

// StageDefinition is the display-safe current definition of one graph node.
type StageDefinition struct {
	Name         string                 `json:"name"`
	Kind         workflow.GraphNodeKind `json:"kind"`
	Goal         string                 `json:"goal"`
	Owner        *GooberReference       `json:"owner"`
	Evaluator    apiv1.EvaluatorKind    `json:"evaluator"`
	Capabilities []string               `json:"capabilities"`
}

// WorkflowDetail adds the canonical graph and stage definitions to the summary.
type WorkflowDetail struct {
	WorkflowSummary
	Graph  workflow.Graph    `json:"graph"`
	Stages []StageDefinition `json:"stages"`
}

type workflowKey struct {
	gaggle string
	name   string
}

type inventoryProjection struct {
	definitions *instance.ConfigSet
	graphs      map[workflowKey]workflow.Graph
	warnings    []validate.CodedWarning
}

func newInventoryProjection(definitions *instance.ConfigSet, report *validate.Report) (*inventoryProjection, error) {
	p := &inventoryProjection{
		definitions: definitions,
		graphs:      make(map[workflowKey]workflow.Graph, len(definitions.Workflows)),
		warnings:    append([]validate.CodedWarning{}, report.Warnings()...),
	}
	goobers := make(map[string]map[string]apiv1.GooberSpec)
	gooberGaggles := make(map[string]string, len(definitions.Goobers))
	for i := range definitions.Goobers {
		def := &definitions.Goobers[i]
		if goobers[def.Spec.Gaggle] == nil {
			goobers[def.Spec.Gaggle] = map[string]apiv1.GooberSpec{}
		}
		goobers[def.Spec.Gaggle][def.Name] = def.Spec
		gooberGaggles[def.Name] = def.Spec.Gaggle
	}

	for i := range definitions.Workflows {
		def := &definitions.Workflows[i]
		key := workflowKey{gaggle: def.Spec.Gaggle, name: def.Name}
		if _, exists := p.graphs[key]; exists {
			return nil, fmt.Errorf("read service: duplicate workflow %q in gaggle %q", def.Name, def.Spec.Gaggle)
		}
		if err := validateWorkflowOwners(def, gooberGaggles); err != nil {
			return nil, err
		}
		machine, err := workflow.Compile(
			workflow.Definition{Name: def.Name, Version: currentWorkflowVersion, Spec: def.Spec},
			workflow.WithGoobers(goobers[def.Spec.Gaggle]),
			workflow.WithPreviewFeatures(
				definitions.Manifest != nil && workflow.PreviewFeaturesEnabled(definitions.Manifest.Annotations),
			),
		)
		if err != nil {
			return nil, fmt.Errorf("read service: compile workflow %q in gaggle %q: %w", def.Name, def.Spec.Gaggle, err)
		}
		p.graphs[key] = machine.Graph()
	}
	return p, nil
}

func validateWorkflowOwners(def *apiv1.Workflow, gooberGaggles map[string]string) error {
	check := func(stage, owner string) error {
		if owner == "" {
			return nil
		}
		gaggle, ok := gooberGaggles[owner]
		if !ok {
			return fmt.Errorf("read service: workflow %q in gaggle %q stage %q references unknown goober %q",
				def.Name, def.Spec.Gaggle, stage, owner)
		}
		if gaggle != def.Spec.Gaggle {
			return fmt.Errorf("read service: workflow %q in gaggle %q stage %q references goober %q in gaggle %q",
				def.Name, def.Spec.Gaggle, stage, owner, gaggle)
		}
		return nil
	}
	for i := range def.Spec.Tasks {
		if err := check(def.Spec.Tasks[i].Name, def.Spec.Tasks[i].Goober); err != nil {
			return err
		}
	}
	for i := range def.Spec.Gates {
		gate := &def.Spec.Gates[i]
		if gate.Agentic != nil {
			if err := check(gate.Name, gate.Agentic.Goober); err != nil {
				return err
			}
		}
	}
	return nil
}

// Instance returns the current overview inventory and warning projection.
func (s *Local) Instance(ctx context.Context) (Instance, error) {
	if err := ctx.Err(); err != nil {
		return Instance{}, err
	}
	inventory := s.definitions.Load().inventory
	active, err := s.activeRunCounts()
	if err != nil {
		return Instance{}, err
	}
	activeTotal := 0
	for _, count := range active {
		activeTotal += count
	}

	ready := s.ready()
	status := InstanceStatusStarting
	if ready {
		status = InstanceStatusReady
		if len(inventory.warnings) > 0 {
			status = InstanceStatusDegraded
		}
	}
	maxConcurrent := 0
	if s.sources.Config != nil {
		maxConcurrent = s.sources.Config.RunConditions.MaxParallelRuns
	}
	return Instance{
		APIVersion:    APIVersion,
		SchemaVersion: SchemaVersion,
		Name:          inventory.definitions.Manifest.Spec.Instance.Name,
		Environment:   inventory.definitions.Manifest.Spec.Instance.Environment,
		Ready:         ready,
		Status:        status,
		Concurrency: Concurrency{
			ActiveRuns:        activeTotal,
			MaxConcurrentRuns: maxConcurrent,
		},
		Counts: InventoryCounts{
			Gaggles:    len(inventory.definitions.Gaggles),
			Goobers:    len(inventory.definitions.Goobers),
			Workflows:  len(inventory.definitions.Workflows),
			ActiveRuns: activeTotal,
		},
		Warnings: append([]validate.CodedWarning{}, inventory.warnings...),
	}, nil
}

// Gaggles returns configured gaggles sorted by identity.
func (s *Local) Gaggles(ctx context.Context, request PageRequest) (GagglePage, error) {
	if err := ctx.Err(); err != nil {
		return GagglePage{}, err
	}
	inventory := s.definitions.Load().inventory
	active, err := s.activeRunCounts()
	if err != nil {
		return GagglePage{}, err
	}
	activeByGaggle := make(map[string]int)
	for identity, count := range active {
		activeByGaggle[identity.Gaggle] += count
	}
	items := make([]Gaggle, 0, len(inventory.definitions.Gaggles))
	for i := range inventory.definitions.Gaggles {
		def := &inventory.definitions.Gaggles[i]
		item := Gaggle{
			Name:           def.Name,
			DisplayName:    displayName(def.Spec.DisplayName, def.Name),
			Status:         DefinitionStatusConfigured,
			Project:        def.Spec.Project,
			Backlog:        def.Spec.Backlog,
			ActiveRunCount: activeByGaggle[def.Name],
			Warnings:       warningsFor(inventory, "Gaggle", def.Name, "", ""),
		}
		for j := range inventory.definitions.Goobers {
			if inventory.definitions.Goobers[j].Spec.Gaggle == def.Name {
				item.GooberCount++
			}
		}
		for j := range inventory.definitions.Workflows {
			wf := &inventory.definitions.Workflows[j]
			if wf.Spec.Gaggle == def.Name {
				item.WorkflowCount++
			}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	page, info, err := paginate(items, request, "gaggles", "", func(item Gaggle) string { return item.Name })
	if err != nil {
		return GagglePage{}, err
	}
	return GagglePage{Items: page, Page: info}, nil
}

// Goobers returns provisioned goober definitions for one gaggle.
func (s *Local) Goobers(ctx context.Context, gaggle string, request PageRequest) (GooberPage, error) {
	if err := ctx.Err(); err != nil {
		return GooberPage{}, err
	}
	inventory := s.definitions.Load().inventory
	if !hasGaggle(inventory, gaggle) {
		return GooberPage{}, fmt.Errorf("%w: gaggle %q", ErrNotFound, gaggle)
	}
	items := make([]Goober, 0)
	for i := range inventory.definitions.Goobers {
		def := &inventory.definitions.Goobers[i]
		if def.Spec.Gaggle != gaggle {
			continue
		}
		harness := def.Spec.Harness
		if harness == "" {
			harness = apiv1.HarnessCopilot
		}
		item := Goober{
			Name:         def.Name,
			DisplayName:  displayName(def.Spec.DisplayName, def.Name),
			Role:         def.Spec.Role,
			Status:       DefinitionStatusConfigured,
			Harness:      harness,
			Skills:       sortedStrings(def.Spec.Skills),
			Capabilities: sortedStrings(def.Spec.Capabilities),
			Workflows:    make([]WorkflowReference, 0),
			Stages:       make([]StageOwnership, 0),
			Warnings:     warningsFor(inventory, "Goober", def.Name, "", ""),
		}
		workflows := make(map[WorkflowReference]bool)
		for _, name := range def.Spec.Workflows {
			workflows[WorkflowReference{Gaggle: gaggle, Name: name}] = true
		}
		for j := range inventory.definitions.Workflows {
			wf := &inventory.definitions.Workflows[j]
			if wf.Spec.Gaggle != gaggle {
				continue
			}
			ref := WorkflowReference{Gaggle: gaggle, Name: wf.Name}
			for _, task := range wf.Spec.Tasks {
				if task.Goober == def.Name {
					workflows[ref] = true
					item.Stages = append(item.Stages, StageOwnership{
						Workflow: ref,
						Stage:    task.Name,
						Kind:     workflow.GraphNodeKind(task.Type),
					})
				}
			}
			for _, gate := range wf.Spec.Gates {
				if gate.Agentic != nil && gate.Agentic.Goober == def.Name {
					workflows[ref] = true
					item.Stages = append(item.Stages, StageOwnership{
						Workflow: ref,
						Stage:    gate.Name,
						Kind:     workflow.GraphNodeGate,
					})
				}
			}
		}
		for ref := range workflows {
			item.Workflows = append(item.Workflows, ref)
		}
		sort.Slice(item.Workflows, func(i, j int) bool { return item.Workflows[i].Name < item.Workflows[j].Name })
		sort.Slice(item.Stages, func(i, j int) bool {
			if item.Stages[i].Workflow.Name != item.Stages[j].Workflow.Name {
				return item.Stages[i].Workflow.Name < item.Stages[j].Workflow.Name
			}
			return item.Stages[i].Stage < item.Stages[j].Stage
		})
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	page, info, err := paginate(items, request, "goobers", gaggle, func(item Goober) string { return item.Name })
	if err != nil {
		return GooberPage{}, err
	}
	return GooberPage{Items: page, Page: info}, nil
}

// Workflows returns current workflow summaries for one gaggle.
func (s *Local) Workflows(ctx context.Context, gaggle string, request PageRequest) (WorkflowPage, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowPage{}, err
	}
	inventory := s.definitions.Load().inventory
	if !hasGaggle(inventory, gaggle) {
		return WorkflowPage{}, fmt.Errorf("%w: gaggle %q", ErrNotFound, gaggle)
	}
	active, err := s.activeRunCounts()
	if err != nil {
		return WorkflowPage{}, err
	}
	items := make([]WorkflowSummary, 0)
	for i := range inventory.definitions.Workflows {
		def := &inventory.definitions.Workflows[i]
		if def.Spec.Gaggle == gaggle {
			items = append(items, s.workflowSummary(inventory, def, active))
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Identity.Name < items[j].Identity.Name })
	page, info, err := paginate(items, request, "workflows", gaggle, func(item WorkflowSummary) string {
		return item.Identity.Name
	})
	if err != nil {
		return WorkflowPage{}, err
	}
	return WorkflowPage{Items: page, Page: info}, nil
}

// Workflow returns current workflow detail, scoped by gaggle.
func (s *Local) Workflow(ctx context.Context, gaggle, name string) (WorkflowDetail, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowDetail{}, err
	}
	inventory := s.definitions.Load().inventory
	if !hasGaggle(inventory, gaggle) {
		return WorkflowDetail{}, fmt.Errorf("%w: gaggle %q", ErrNotFound, gaggle)
	}
	var def *apiv1.Workflow
	for i := range inventory.definitions.Workflows {
		candidate := &inventory.definitions.Workflows[i]
		if candidate.Spec.Gaggle == gaggle && candidate.Name == name {
			def = candidate
			break
		}
	}
	if def == nil {
		return WorkflowDetail{}, fmt.Errorf("%w: workflow %q in gaggle %q", ErrNotFound, name, gaggle)
	}
	active, err := s.activeRunCounts()
	if err != nil {
		return WorkflowDetail{}, err
	}
	return WorkflowDetail{
		WorkflowSummary: s.workflowSummary(inventory, def, active),
		Graph:           inventory.graphs[workflowKey{gaggle: gaggle, name: name}],
		Stages:          workflowStages(def),
	}, nil
}

func (s *Local) activeRunCounts() (map[localscheduler.WorkflowIdentity]int, error) {
	runDirs, err := s.sources.Layout.RunDirs()
	if err != nil {
		return nil, fmt.Errorf("enumerate run roots: %w", err)
	}
	counts, err := localscheduler.ActiveRunCountsByWorkflowDirs(runDirs)
	if err != nil {
		return nil, fmt.Errorf("read active run projection: %w", err)
	}
	return counts, nil
}

func hasGaggle(inventory *inventoryProjection, name string) bool {
	for i := range inventory.definitions.Gaggles {
		if inventory.definitions.Gaggles[i].Name == name {
			return true
		}
	}
	return false
}

func (s *Local) workflowSummary(inventory *inventoryProjection, def *apiv1.Workflow, active map[localscheduler.WorkflowIdentity]int) WorkflowSummary {
	key := workflowKey{gaggle: def.Spec.Gaggle, name: def.Name}
	graph := inventory.graphs[key]
	readiness := def.Spec.Readiness
	if readiness.MaxConcurrentRuns <= 0 {
		readiness.MaxConcurrentRuns = 1
	}
	if readiness.MaxRunsPerHour <= 0 {
		readiness.MaxRunsPerHour = 10
	}
	if s.sources.Config != nil {
		if override := s.sources.Config.RunConditions.WorkflowBudgets[def.Name]; override > 0 {
			readiness.MaxRunsPerHour = int32(override)
		}
		if override := s.sources.Config.RunConditions.WorkflowDailyBudgets[def.Name]; override > 0 {
			readiness.MaxRunsPerDay = int32(override)
		}
	}
	owners := make([]GooberReference, 0)
	seenOwners := make(map[string]bool)
	for _, node := range graph.Nodes {
		if node.Owner != "" && !seenOwners[node.Owner] {
			seenOwners[node.Owner] = true
			owners = append(owners, GooberReference{Gaggle: def.Spec.Gaggle, Name: node.Owner})
		}
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i].Name < owners[j].Name })
	triggers := append([]apiv1.Trigger{}, def.Spec.Triggers...)
	purpose := ""
	if def.Annotations != nil {
		purpose = def.Annotations["goobers.dev/purpose"]
	}
	return WorkflowSummary{
		Identity:    WorkflowReference{Gaggle: def.Spec.Gaggle, Name: def.Name},
		DisplayName: displayName(def.Spec.DisplayName, def.Name),
		Purpose:     purpose,
		Triggers:    triggers,
		Readiness:   readiness,
		Concurrency: WorkflowConcurrency{
			ActiveRuns:        int32(active[localscheduler.WorkflowIdentity{Gaggle: def.Spec.Gaggle, Workflow: def.Name}]),
			MaxConcurrentRuns: readiness.MaxConcurrentRuns,
		},
		Owners:     owners,
		StageCount: len(graph.Nodes),
		Definition: WorkflowDefinition{Version: graph.Version, Digest: graph.Digest},
		Warnings:   workflowWarnings(inventory, def),
	}
}

func workflowStages(def *apiv1.Workflow) []StageDefinition {
	stages := make([]StageDefinition, 0, len(def.Spec.Tasks)+len(def.Spec.Gates))
	for _, task := range def.Spec.Tasks {
		var owner *GooberReference
		if task.Goober != "" {
			ref := GooberReference{Gaggle: def.Spec.Gaggle, Name: task.Goober}
			owner = &ref
		}
		stages = append(stages, StageDefinition{
			Name:         task.Name,
			Kind:         workflow.GraphNodeKind(task.Type),
			Goal:         task.Goal,
			Owner:        owner,
			Capabilities: sortedStrings(task.Capabilities),
		})
	}
	for _, gate := range def.Spec.Gates {
		var owner *GooberReference
		if gate.Agentic != nil && gate.Agentic.Goober != "" {
			ref := GooberReference{Gaggle: def.Spec.Gaggle, Name: gate.Agentic.Goober}
			owner = &ref
		}
		stages = append(stages, StageDefinition{
			Name:         gate.Name,
			Kind:         workflow.GraphNodeGate,
			Owner:        owner,
			Evaluator:    gate.Evaluator,
			Capabilities: []string{},
		})
	}
	return stages
}

func workflowWarnings(inventory *inventoryProjection, def *apiv1.Workflow) []validate.CodedWarning {
	source, _ := inventory.definitions.WorkflowSource(def.Spec.Gaggle, def.Name)
	return warningsFor(inventory, "Workflow", def.Name, source, "Gaggle/"+def.Spec.Gaggle)
}

func warningsFor(inventory *inventoryProjection, kind, name, source, parentScope string) []validate.CodedWarning {
	if source == "" && definitionNameCount(inventory, kind, name) != 1 {
		return []validate.CodedWarning{}
	}
	needle := kind + "/" + name
	if parentScope != "" {
		needle = parentScope + " " + needle
	}
	if source != "" {
		needle = source + " " + needle
	}
	warnings := make([]validate.CodedWarning, 0)
	for _, warning := range inventory.warnings {
		if warning.Scope == needle || (source == "" && strings.HasSuffix(warning.Scope, " "+needle)) {
			warnings = append(warnings, warning)
		}
	}
	return warnings
}

func definitionNameCount(inventory *inventoryProjection, kind, name string) int {
	count := 0
	switch kind {
	case "Gaggle":
		for i := range inventory.definitions.Gaggles {
			if inventory.definitions.Gaggles[i].Name == name {
				count++
			}
		}
	case "Goober":
		for i := range inventory.definitions.Goobers {
			if inventory.definitions.Goobers[i].Name == name {
				count++
			}
		}
	case "Workflow":
		for i := range inventory.definitions.Workflows {
			if inventory.definitions.Workflows[i].Name == name {
				count++
			}
		}
	}
	return count
}

func displayName(configured, fallback string) string {
	if configured != "" {
		return configured
	}
	return fallback
}

func sortedStrings(values []string) []string {
	result := append([]string{}, values...)
	sort.Strings(result)
	return result
}

type pageCursor struct {
	Collection string `json:"collection"`
	Scope      string `json:"scope"`
	After      string `json:"after"`
}

func paginate[T any](items []T, request PageRequest, collection, scope string, key func(T) string) ([]T, PageInfo, error) {
	limit := request.Limit
	if limit == 0 {
		limit = DefaultPageSize
	}
	if limit < 1 || limit > MaxPageSize {
		return nil, PageInfo{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidPage, MaxPageSize)
	}

	start := 0
	if request.Cursor != "" {
		cursor, err := decodeCursor(request.Cursor)
		if err != nil || cursor.Collection != collection || cursor.Scope != scope {
			return nil, PageInfo{}, ErrInvalidCursor
		}
		found := false
		for i := range items {
			if key(items[i]) == cursor.After {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return nil, PageInfo{}, ErrInvalidCursor
		}
	}

	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	page := append([]T{}, items[start:end]...)
	info := PageInfo{Limit: limit, Total: len(items), HasMore: end < len(items)}
	if info.HasMore {
		info.NextCursor = encodeCursor(pageCursor{
			Collection: collection,
			Scope:      scope,
			After:      key(items[end-1]),
		})
	}
	return page, info, nil
}

func encodeCursor(cursor pageCursor) string {
	data, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeCursor(encoded string) (pageCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return pageCursor{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var cursor pageCursor
	if err := decoder.Decode(&cursor); err != nil {
		return pageCursor{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return pageCursor{}, errors.New("cursor has trailing data")
	}
	if cursor.Collection == "" || cursor.After == "" {
		return pageCursor{}, errors.New("cursor fields are required")
	}
	return cursor, nil
}
