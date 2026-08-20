package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

func TestFeatureMatrixDocUpToDate(t *testing.T) {
	dir := docsDir(t)
	path := filepath.Join(dir, featureMatrixFile)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := writeFeatureMatrix(dir); err != nil {
			t.Fatalf("writeFeatureMatrix: %v", err)
		}
		return
	}

	want, err := renderFeatureMatrix()
	if err != nil {
		t.Fatalf("renderFeatureMatrix: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", featureMatrixFile, err)
	}
	if string(got) != want {
		t.Fatalf("docs/%s is out of date; regenerate with make docs", featureMatrixFile)
	}
}

func TestFeatureMatrixCoversEveryFeature(t *testing.T) {
	doc, err := renderFeatureMatrix()
	if err != nil {
		t.Fatal(err)
	}
	features := workflow.AllFeatures()
	if len(features) == 0 {
		t.Fatal("registry returned no features")
	}
	for _, feature := range features {
		if !strings.Contains(doc, "`"+string(feature.ID)+"`") {
			t.Errorf("feature %q missing from the generated matrix", feature.ID)
		}
	}
}

func TestFeatureRegistryCoversSpecFields(t *testing.T) {
	mapped := map[string][]workflow.FeatureID{
		"WorkflowSpec.Gaggle":      {"workflow.spec.gaggle"},
		"WorkflowSpec.DisplayName": {"workflow.spec.displayName"},
		"WorkflowSpec.Triggers":    {"workflow.spec.triggers"},
		"WorkflowSpec.Readiness":   {"workflow.spec.readiness"},
		"WorkflowSpec.RunControls": {
			"workflow.spec.runControls",
			"workflow.spec.runControls.maxRepasses",
			"workflow.spec.runControls.stalledRunTimeout",
			"workflow.spec.runControls.maxRunDuration",
		},
		"WorkflowSpec.OutboxMirrorPath": {"workflow.spec.outboxMirrorPath"},
		"WorkflowSpec.Start":            {"workflow.spec.start"},
		"WorkflowSpec.DocsRoots":        {"workflow.spec.docsRoots"},
		"WorkflowSpec.TutorScope": {
			"workflow.spec.tutorScope",
			"workflow.spec.tutorScope.tier",
			"workflow.spec.tutorScope.target",
		},
		"WorkflowSpec.Requires":  {"workflow.spec.requires.capabilities"},
		"WorkflowSpec.Tasks":     {"workflow.spec.tasks"},
		"WorkflowSpec.Gates":     {"workflow.spec.gates"},
		"WorkflowSpec.Parallels": {"workflow.spec.parallels"},

		"GaggleSpec.DisplayName":  {"gaggle.spec.displayName"},
		"GaggleSpec.SelfIdentity": {"gaggle.spec.selfIdentity"},
		"GaggleSpec.Project": {
			"gaggle.spec.project",
			"gaggle.spec.project.provider.github",
			"gaggle.spec.project.provider.ado",
			"gaggle.spec.project.provider.gitea",
			"gaggle.spec.project.baseUrl",
			"gaggle.spec.project.checkout.sparse",
		},
		"GaggleSpec.Backlog": {
			"gaggle.spec.backlog",
			"gaggle.spec.backlog.provider.github",
			"gaggle.spec.backlog.provider.ado",
			"gaggle.spec.backlog.provider.gitea",
			"gaggle.spec.backlog.baseUrl",
			"gaggle.spec.backlog.labels",
			"gaggle.spec.backlog.labelPredicate",
			"gaggle.spec.backlog.fieldPredicate",
		},
		"GaggleSpec.Isolation": {
			"gaggle.spec.isolation.namespace",
			"gaggle.spec.isolation.identityRef",
		},
		"GaggleSpec.AdditionalRepos": {
			"gaggle.spec.additionalRepos",
			"gaggle.spec.additionalRepos.provider.github",
			"gaggle.spec.additionalRepos.provider.ado",
			"gaggle.spec.additionalRepos.provider.gitea",
			"gaggle.spec.additionalRepos.baseUrl",
			"gaggle.spec.additionalRepos.checkout.sparse",
		},
		"GaggleSpec.CICommand":            {"gaggle.spec.ciCommand"},
		"GaggleSpec.RequiredCapabilities": {"gaggle.spec.requiredCapabilities"},
		"GaggleSpec.BranchNamespace":      {"gaggle.spec.branchNamespace"},
		"GaggleSpec.RunControls": {
			"gaggle.spec.runControls",
			"gaggle.spec.runControls.maxRepasses",
			"gaggle.spec.runControls.stalledRunTimeout",
			"gaggle.spec.runControls.maxRunDuration",
		},
		"GaggleSpec.OutboxMirrorPath": {"gaggle.spec.outboxMirrorPath"},
		"GaggleSpec.Sandbox":          {"gaggle.spec.sandbox"},
		"GaggleSpec.Workcopies":       {"gaggle.spec.workcopies.root"},
		"GaggleSpec.RequireLabels":    {"gaggle.spec.requireLabels"},
		"GaggleSpec.Siblings":         {"gaggle.spec.siblings"},

		"GooberSpec.Gaggle":                   {"goober.spec.gaggle"},
		"GooberSpec.Role":                     {"goober.spec.role"},
		"GooberSpec.DisplayName":              {"goober.spec.displayName"},
		"GooberSpec.Instructions":             {"goober.spec.instructions"},
		"GooberSpec.Harness":                  {"goober.spec.harness.copilot", "goober.spec.harness.claude-code"},
		"GooberSpec.Model":                    {"goober.spec.model"},
		"GooberSpec.HarnessOptions":           {"goober.spec.harnessOptions"},
		"GooberSpec.TimeoutSeconds":           {"goober.spec.timeoutSeconds"},
		"GooberSpec.Capabilities":             {"goober.spec.capabilities"},
		"GooberSpec.Skills":                   {"goober.spec.skills"},
		"GooberSpec.Tools":                    {"goober.spec.tools"},
		"GooberSpec.MCPServers":               {"goober.spec.mcpServers"},
		"GooberSpec.PolicyActions":            {"goober.spec.policyActions"},
		"GooberSpec.ConditionalPolicyActions": {"goober.spec.conditionalPolicyActions"},
		"GooberSpec.ScaleFactor":              {"goober.spec.scaleFactor"},
		"GooberSpec.Workflows":                {"goober.spec.workflows"},

		"Trigger.Type":           {"trigger.manual", "trigger.backlog-item", "trigger.schedule", "trigger.signal", "trigger.webhook"},
		"Trigger.Selector":       {"trigger.backlog-item.selector"},
		"Trigger.TrustLabel":     {"trigger.backlog-item.trustLabel"},
		"Trigger.LabelPredicate": {"trigger.labelPredicate"},
		"Trigger.FieldPredicate": {"trigger.fieldPredicate"},
		"Trigger.Priority":       {"trigger.priority"},
		"Trigger.Schedule":       {"trigger.schedule"},
		"Trigger.IdleBackoff": {
			"trigger.idleBackoff",
			"trigger.idleBackoff.enabled",
			"trigger.idleBackoff.floor",
			"trigger.idleBackoff.ceiling",
		},
		"Trigger.Signal": {"trigger.signal"},
		"Trigger.Events": {"trigger.webhook"},

		"Task.Name":                 {"task.name"},
		"Task.Type":                 {"task.deterministic", "task.agentic"},
		"Task.Goal":                 {"task.goal"},
		"Task.Goober":               {"task.goober"},
		"Task.Run":                  {"stage.run.command", "stage.run.script"},
		"Task.Inputs":               {"task.inputs"},
		"Task.Capabilities":         {"task.capabilities"},
		"Task.MinimumIntegrity":     {"task.minimumIntegrity"},
		"Task.ContextFrom":          {"task.contextFrom"},
		"Task.PolicyActions":        {"task.policyActions"},
		"Task.Retry":                {"task.retry"},
		"Task.TimeoutSeconds":       {"task.timeoutSeconds"},
		"Task.Limits":               {"task.limits"},
		"Task.OnTimeout":            {"task.onTimeout.fail", "task.onTimeout.salvage"},
		"Task.ExpectedOutputs":      {"task.expectedOutputs"},
		"Task.ContinueOnError":      {"task.continueOnError"},
		"Task.InputsFrom":           {"task.inputsFrom"},
		"Task.RequiredCapabilities": {"task.requiredCapabilities"},
		"Task.Outbox":               {"task.outbox"},
		"Task.OutboxMirrorPath":     {"task.outboxMirrorPath"},
		"Task.Workspace":            {"stage.workspace"},
		"Task.Next":                 {"task.next"},
	}
	// The feature registry covers EVERY author-facing spec field (#3292, the
	// PO-ruled reversal of #3003's operational-metadata exclusion): an
	// unregistered field carries no declared compatibility promise at all,
	// and the excluded fields were exactly the ones where that absence bit —
	// capability-requirement changes and outboxMirrorPath both shipped with
	// zero version linkage. The map stays so a future field without a mapping
	// still fails this guard until someone writes down a reason, but no
	// current field qualifies.
	excluded := map[string]string{}

	registry := make(map[workflow.FeatureID]struct{})
	for _, feature := range workflow.AllFeatures() {
		registry[feature.ID] = struct{}{}
	}
	for _, typ := range []reflect.Type{
		reflect.TypeOf(apiv1.WorkflowSpec{}),
		reflect.TypeOf(apiv1.GaggleSpec{}),
		reflect.TypeOf(apiv1.GooberSpec{}),
		reflect.TypeOf(apiv1.Trigger{}),
		reflect.TypeOf(apiv1.Task{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			key := typ.Name() + "." + typ.Field(i).Name
			ids, ok := mapped[key]
			if !ok {
				if _, excluded := excluded[key]; !excluded {
					t.Errorf("%s maps to neither a FeatureID nor an explicit exclusion", key)
				}
				continue
			}
			for _, id := range ids {
				if _, ok := registry[id]; !ok {
					t.Errorf("%s maps to missing FeatureID %q", key, id)
				}
			}
		}
	}
	if _, ok := registry["task.inputs.fieldOrder"]; !ok {
		t.Error("validated task input fieldOrder maps to missing FeatureID task.inputs.fieldOrder")
	}
}

func TestFeatureMatrixMatchesBinaryReport(t *testing.T) {
	doc, err := renderFeatureMatrix()
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runArgs(t, "features")
	if code != 0 {
		t.Fatalf("features exited %d: %s", code, stderr)
	}
	docRows := parseFeatureDocRows(t, doc)
	reportRows := parseFeatureReportRows(t, stdout)
	if len(reportRows) != len(docRows) {
		t.Fatalf("features reported %d rows, generated doc contains %d", len(reportRows), len(docRows))
	}
	for i := range docRows {
		if reportRows[i] != docRows[i] {
			t.Errorf("row %d differs:\nfeatures:      %+v\ngenerated doc: %+v", i+1, reportRows[i], docRows[i])
		}
	}
}

type featureReportRow struct {
	Feature        string
	DSLVersion     string
	FeatureSupport string
	VersionSupport string
	Since          string
}

func parseFeatureDocRows(t *testing.T, doc string) []featureReportRow {
	t.Helper()
	const header = "| Feature | DSL version | Feature support | Version support | Since app version |"
	const separator = "| --- | --- | --- | --- | --- |"
	lines := strings.Split(doc, "\n")
	headerIndex := -1
	for i, line := range lines {
		if line == header {
			headerIndex = i
			break
		}
	}
	if headerIndex < 0 || headerIndex+2 >= len(lines) {
		t.Fatalf("generated doc is missing the feature matrix header")
	}
	if lines[headerIndex+1] != separator {
		t.Fatalf("unexpected generated doc table separator %q", lines[headerIndex+1])
	}

	var rows []featureReportRow
	for _, line := range lines[headerIndex+2:] {
		if line == "" {
			break
		}
		cells := strings.Split(line, "|")
		if len(cells) != 7 || strings.TrimSpace(cells[0]) != "" || strings.TrimSpace(cells[6]) != "" {
			t.Fatalf("invalid generated doc row %q", line)
		}
		for i := 1; i <= 5; i++ {
			cells[i] = strings.TrimSpace(cells[i])
		}
		featureCell := cells[1]
		if len(featureCell) < 3 || featureCell[0] != '`' || featureCell[len(featureCell)-1] != '`' {
			t.Fatalf("invalid feature cell %q in generated doc row %q", cells[1], line)
		}
		feature := featureCell[1 : len(featureCell)-1]
		rows = append(rows, featureReportRow{
			Feature:        feature,
			DSLVersion:     cells[2],
			FeatureSupport: cells[3],
			VersionSupport: cells[4],
			Since:          cells[5],
		})
	}
	return rows
}

func parseFeatureReportRows(t *testing.T, report string) []featureReportRow {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(report), "\n")
	if len(lines) < 3 {
		t.Fatalf("features output is incomplete:\n%s", report)
	}
	wantHeader := []string{"FEATURE", "DSL", "VERSION", "FEATURE", "SUPPORT", "VERSION", "SUPPORT", "SINCE"}
	if got := strings.Fields(lines[0]); strings.Join(got, "\t") != strings.Join(wantHeader, "\t") {
		t.Fatalf("unexpected features header %q", lines[0])
	}

	separator := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			separator = i
			break
		}
	}
	if separator < 0 || separator != len(lines)-2 {
		t.Fatalf("features output must contain exactly one footer after the table:\n%s", report)
	}

	rows := make([]featureReportRow, 0, separator-1)
	for _, line := range lines[1:separator] {
		fields := strings.Fields(line)
		if len(fields) != 5 {
			t.Fatalf("invalid features row %q", line)
		}
		rows = append(rows, featureReportRow{
			Feature:        fields[0],
			DSLVersion:     fields[1],
			FeatureSupport: fields[2],
			VersionSupport: fields[3],
			Since:          fields[4],
		})
	}
	if got, want := strings.TrimSpace(lines[len(lines)-1]), fmt.Sprintf("%d feature/version row(s)", len(rows)); got != want {
		t.Fatalf("features footer = %q, want %q", got, want)
	}
	return rows
}

func TestFeatureVersionDelta(t *testing.T) {
	features := []workflow.Feature{
		{ID: "added", DSLVersions: []workflow.DSLFeatureSupport{{Version: "1.1", Level: workflow.SupportPreview}}},
		{ID: "removed", DSLVersions: []workflow.DSLFeatureSupport{{Version: "1.0", Level: workflow.SupportGA}}},
		{ID: "changed", DSLVersions: []workflow.DSLFeatureSupport{
			{Version: "1.0", Level: workflow.SupportPreview},
			{Version: "1.1", Level: workflow.SupportGA},
		}},
	}
	delta, err := featureVersionDelta(features, "1.0", "1.1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(delta.Added, ","), "added"; got != want {
		t.Errorf("added = %q, want %q", got, want)
	}
	if got, want := strings.Join(delta.Removed, ","), "removed"; got != want {
		t.Errorf("removed = %q, want %q", got, want)
	}
	if got, want := strings.Join(delta.LevelChanges, ","), "changed (preview -> ga)"; got != want {
		t.Errorf("level changes = %q, want %q", got, want)
	}
}
