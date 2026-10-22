package main

import (
	"maps"
	"strings"
	"testing"
)

func assertCIJobCancellationGroups(t *testing.T, w ciWorkflow, ctx map[string]any, prefix string) {
	t.Helper()
	if w.Concurrency.Group != "" || w.Concurrency.CancelInProgress {
		t.Fatal("a workflow-wide concurrency slot would let an always() reporter block replacement validation")
	}
	ctx = maps.Clone(ctx)
	ctx["matrix"] = map[string]any{"goos": "linux", "os": "ubuntu-latest", "shard": "1/5"}
	suffix := map[string]string{"lint": "-linux", "shipped": "-ubuntu-latest", "unit": "-1/5"}
	for id, job := range w.Jobs {
		if id == "required-ci" || id == "escalate-main-failure" || strings.HasPrefix(id, "cancel-on-") {
			if job.Concurrency.Group != "" || job.Concurrency.CancelInProgress {
				t.Errorf("reporter/canceller %s must not cancel another run's report", id)
			}
			continue
		}
		want := prefix + "-" + id + suffix[id]
		if got := renderTemplate(t, job.Concurrency.Group, ctx); got != want || !job.Concurrency.CancelInProgress {
			t.Errorf("%s concurrency = %q, want cancelling group %q", id, got, want)
		}
	}
}

func TestCIJobCancellationSharesPRAndRetargetLanes(t *testing.T) {
	t.Parallel()
	w := loadCIWorkflow(t)
	for _, tc := range []struct {
		ref, sha, pr, want string
	}{
		{"refs/pull/1/merge", fixtureBaseSHA, "", "refs/pull/1/merge"},
		{"refs/pull/1/merge", fixtureHeadSHA, "", "refs/pull/1/merge"},
		{"refs/heads/feature", fixtureHeadSHA, "1", "refs/pull/1/merge"},
		{"refs/pull/2/merge", fixtureHeadSHA, "", "refs/pull/2/merge"},
		{"refs/heads/main", fixtureHeadSHA, "", "refs/heads/main"},
		{"refs/heads/qualification", fixtureHeadSHA, "", "refs/heads/qualification"},
	} {
		ctx := map[string]any{
			"github": map[string]any{"workflow": "CI", "ref": tc.ref, "sha": tc.sha},
			"inputs": map[string]any{"pr_number": tc.pr},
		}
		assertCIJobCancellationGroups(t, w, ctx, "ci-CI-"+tc.want)
	}
}

func TestCIJobCancellationDoesNotCancelSiblingMatrixLegs(t *testing.T) {
	t.Parallel()
	w := loadCIWorkflow(t)
	for id, matrix := range map[string]struct {
		key    string
		values []string
	}{
		"lint":    {"goos", []string{"linux", "darwin", "windows"}},
		"shipped": {"os", []string{"ubuntu-latest", "windows-latest"}},
		"unit":    {"shard", w.Jobs["unit"].Strategy.Matrix.Shard},
	} {
		seen := map[string]bool{}
		for _, value := range matrix.values {
			ctx := map[string]any{
				"github": map[string]any{"workflow": "CI", "ref": "refs/pull/1/merge"},
				"matrix": map[string]any{matrix.key: value},
			}
			group := renderTemplate(t, w.Jobs[id].Concurrency.Group, ctx)
			if seen[group] {
				t.Errorf("%s sibling %s shares cancelling group %q", id, value, group)
			}
			seen[group] = true
		}
	}
}

func TestCICancellationCannotSkipCurrentHeadRequiredCheck(t *testing.T) {
	t.Parallel()
	aggregate := loadCIWorkflow(t).Jobs["required-ci"]
	for _, event := range []string{"pull_request", "workflow_dispatch", "merge_group"} {
		for _, result := range []string{"success", "failure", "cancelled", "skipped"} {
			for _, cancelled := range []bool{false, true} {
				ctx := map[string]any{
					"github": map[string]any{"event_name": event, "sha": fixtureHeadSHA},
					"needs":  map[string]any{"unit": map[string]any{"result": result}},
				}
				cond := trimExpr(aggregate.If)
				run := truthy(evalExpr(t, cond, ctx, result == "success", cancelled))
				if !hasStatusFunction(cond) {
					run = run && result == "success"
				}
				if !run {
					t.Errorf("%s/%s cancelled=%v: current-SHA aggregate must report, never skip", event, result, cancelled)
				}
			}
		}
	}
	gate := aggregate.step(t, "Verify required gates")
	if aggregate.Name != pinnedRequiredCheck || aggregate.ContinueOnError ||
		gate.Run != "go run ./test/cipolicy gate" || gate.If != "always()" || gate.ContinueOnError {
		t.Fatal("the pinned current-SHA check must retain the unconditional fail-closed gate")
	}
	veto := aggregate.step(t, "Reject cancelled validation")
	if veto.If != "cancelled()" || veto.ContinueOnError || veto.Env["CI_CANCELLED"] != "true" ||
		veto.Run != "go run ./test/cipolicy gate" || aggregate.Steps[len(aggregate.Steps)-1].Name != veto.Name {
		t.Fatal("a final cancellation veto must reject an otherwise successful aggregate")
	}
	for _, cancelled := range []bool{false, true} {
		if got := truthy(evalExpr(t, veto.If, nil, true, cancelled)); got != cancelled {
			t.Fatal("cancellation veto does not follow workflow cancellation")
		}
	}
}

func TestMainEscalationRetainsLatestCommitSignal(t *testing.T) {
	t.Parallel()
	job := loadCIWorkflow(t).Jobs["escalate-main-failure"]
	guard := job.step(t, "Check latest main commit")
	if guard.ID != "latest-main" || guard.ContinueOnError || guard.If != "" ||
		!strings.Contains(guard.with("script"), "github.rest.repos.getBranch") ||
		!strings.Contains(guard.with("script"), `branch: "main"`) ||
		!strings.Contains(guard.with("script"), "String(branch.commit.sha === context.sha)") {
		t.Fatal("escalation must verify its event commit against current main")
	}
	for _, latest := range []bool{false, true} {
		for _, result := range []string{"success", "failure", "cancelled"} {
			needs := map[string]any{}
			for _, id := range job.Needs {
				needs[id] = map[string]any{"result": result}
			}
			isLatest := "false"
			if latest {
				isLatest = "true"
			}
			ctx := map[string]any{
				"github": map[string]any{"event_name": "push", "ref": "refs/heads/main"}, "needs": needs,
				"steps": map[string]any{"latest-main": map[string]any{"outputs": map[string]any{"result": isLatest}}},
			}
			if !truthy(evalExpr(t, trimExpr(job.If), ctx, result == "success")) {
				t.Fatal("main escalation must remain eligible after validation failures")
			}
			for name, want := range map[string]bool{
				"File or update critical main failure": latest && result == "failure",
				"Close resolved main failure":          latest && result == "success",
			} {
				if got := truthy(evalExpr(t, trimExpr(job.step(t, name).If), ctx, true)); got != want {
					t.Errorf("latest=%v result=%s step=%s got=%v want=%v", latest, result, name, got, want)
				}
			}
		}
	}
}

func TestCIStatusFunctionsStayOutOfEnvironmentExpressions(t *testing.T) {
	t.Parallel()
	w := loadCIWorkflow(t)
	check := func(env map[string]string) {
		for key, value := range env {
			if strings.Contains(value, "${{") && hasStatusFunction(value) {
				t.Errorf("env %s uses a status function outside an if condition: %s", key, value)
			}
		}
	}
	check(w.Env)
	for _, job := range w.Jobs {
		check(job.Env)
		for _, step := range job.Steps {
			check(step.Env)
		}
	}
}
