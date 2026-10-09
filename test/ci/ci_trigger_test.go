package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

const ciFullGate = "${{ needs.scope.outputs.profile == 'full' }}"

const pinnedRequiredCheck = "make ci (fmt-check · vet · build · test · lint)"

// ciEventFixture is one GitHub event as ci.yml's expressions see it.
type ciEventFixture struct {
	name  string
	event string
	// payload is github.event: only the fields the workflow reads.
	payload map[string]any
	// validates: the event starts a CI run that executes the full required
	// gate under the pinned required-check name, in its ref's shared
	// (cancelling) concurrency group. When false it must not start CI at all.
	validates bool
}

func ciEventFixtures() []ciEventFixture {
	pr := func(action string, changes map[string]any) map[string]any {
		payload := map[string]any{"action": action}
		if changes != nil {
			payload["changes"] = changes
		}
		return payload
	}
	from := func(v string) map[string]any { return map[string]any{"from": v} }
	base := map[string]any{"ref": from("release"), "sha": from("0123abc")}
	return []ciEventFixture{
		{"pull request opened", "pull_request", pr("opened", nil), true},
		{"new head pushed", "pull_request", pr("synchronize", nil), true},
		{"pull request reopened", "pull_request", pr("reopened", nil), true},
		{"title-only edit", "pull_request", pr("edited", map[string]any{"title": from("old")}), false},
		{"body-only edit", "pull_request", pr("edited", map[string]any{"body": from("old")}), false},
		// A retarget is an `edited` event too. Re-running CI against the new
		// merge result without an edited CI run is follow-up #7044; until then
		// the next push re-runs CI.
		{"base retarget", "pull_request", pr("edited", map[string]any{"base": base}), false},
		{"base retarget with title edit", "pull_request", pr("edited", map[string]any{"base": base, "title": from("old")}), false},
		{"merge queue", "merge_group", map[string]any{"action": "checks_requested"}, true},
		{"manual dispatch", "workflow_dispatch", map[string]any{}, true},
	}
}

// TestCIIgnoresPullRequestEdits evaluates ci.yml's trigger, concurrency, job
// conditions and required-check name against event fixtures (#7017, #5491).
// A pull-request `edited` event must not start CI at all: any CI run on it,
// even one whose jobs all skip, cancels the in-flight run or adds a newer CI
// check suite to the head SHA, and a PR whose newest CI suite came from an
// edit stays BLOCKED despite a green required check (observed GitHub
// behaviour, #7017). Every code event starts the full gate under the pinned
// required name.
func TestCIIgnoresPullRequestEdits(t *testing.T) {
	t.Parallel()
	w := loadCIWorkflow(t)
	if types := pullRequestTypes(t, w); slices.Contains(types, "edited") {
		t.Fatalf("ci.yml pull_request types %v include edited; a title/body edit would add a CI suite that blocks the PR", types)
	}
	for _, fx := range ciEventFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			triggered := workflowTriggered(t, w, fx)
			if !fx.validates {
				if triggered {
					t.Fatal("a pull-request edit starts a CI run; it must start none")
				}
				return
			}
			if !triggered {
				t.Fatal("event does not start CI")
			}
			ctx := map[string]any{"github": map[string]any{
				"event_name": fx.event, "event": fx.payload, "workflow": "CI",
				"ref": "refs/pull/1/merge", "run_id": "4242",
			}}
			if group, want := renderTemplate(t, w.Concurrency.Group, ctx), "ci-CI-refs/pull/1/merge"; group != want {
				t.Errorf("concurrency group = %q, want the ref's shared group %q so it replaces the stale run", group, want)
			}
			ran := simulateCIJobs(t, w, ctx)
			for _, id := range append([]string{"required-ci"}, w.Jobs["required-ci"].Needs...) {
				if !ran[id] {
					t.Errorf("required job %s does not run", id)
				}
			}
			if name := renderTemplate(t, w.Jobs["required-ci"].Name, ctx); name != pinnedRequiredCheck {
				t.Errorf("required-ci renders %q, want the ruleset-pinned %q", name, pinnedRequiredCheck)
			}
		})
	}
}

// workflowTriggered reports whether w's `on:` fires for the fixture event,
// honouring pull_request activity types.
func workflowTriggered(t *testing.T, w ciWorkflow, fx ciEventFixture) bool {
	t.Helper()
	if _, ok := w.On[fx.event]; !ok {
		return false
	}
	if fx.event != "pull_request" {
		return true
	}
	types := pullRequestTypes(t, w)
	if len(types) == 0 {
		types = []string{"opened", "synchronize", "reopened"} // GitHub's default
	}
	return slices.Contains(types, fx.payload["action"].(string))
}

func pullRequestTypes(t *testing.T, w ciWorkflow) []string {
	t.Helper()
	var types []string
	trigger := w.On["pull_request"]
	if err := trigger.Decode(&struct {
		Types *[]string `yaml:"types"`
	}{&types}); err != nil {
		t.Fatalf("decode pull_request trigger: %v", err)
	}
	return types
}

func trimExpr(cond string) string {
	cond = strings.TrimSpace(cond)
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(cond, "${{"), "}}"))
}

// requiredCIDisplayName is required-ci's check name on a run that validates
// code (a new pull-request head), which is the context the ruleset requires.
func requiredCIDisplayName(t *testing.T, w ciWorkflow) string {
	t.Helper()
	ctx := map[string]any{"github": map[string]any{
		"event_name": "pull_request", "event": map[string]any{"action": "synchronize"},
	}}
	return renderTemplate(t, w.Jobs["required-ci"].Name, ctx)
}

// simulateCIJobs reports which jobs run for one event when every job that runs
// succeeds, applying GitHub's rule that a condition without a status function
// is implicitly `success() && ...`.
func simulateCIJobs(t *testing.T, w ciWorkflow, ctx map[string]any) map[string]bool {
	t.Helper()
	return simulateCIJobsWithProfile(t, w, ctx, "full")
}

func simulateCIJobsWithProfile(t *testing.T, w ciWorkflow, ctx map[string]any, profile string) map[string]bool {
	t.Helper()
	ran := map[string]bool{}
	var visit func(id string) bool
	visit = func(id string) bool {
		if r, ok := ran[id]; ok {
			return r
		}
		job := w.Jobs[id]
		needs := map[string]any{}
		allOK := true
		for _, dep := range job.Needs {
			result := "skipped"
			if visit(dep) {
				result = "success"
			}
			allOK = allOK && result == "success"
			outputs := map[string]any{}
			if dep == "scope" && result == "success" {
				outputs["profile"] = profile
			}
			needs[dep] = map[string]any{"result": result, "outputs": outputs}
		}
		cond := trimExpr(job.If)
		if cond == "" {
			cond = "success()"
		}
		jobCtx := map[string]any{"github": ctx["github"], "needs": needs}
		run := truthy(evalExpr(t, cond, jobCtx, allOK))
		if !hasStatusFunction(cond) {
			run = run && allOK
		}
		ran[id] = run
		return run
	}
	for id := range w.Jobs {
		visit(id)
	}
	return ran
}

func TestCIScopePreservesPortalUXAndSelectsOnlyBackendSkips(t *testing.T) {
	t.Parallel()
	w := loadCIWorkflow(t)
	for _, action := range []string{"opened", "synchronize", "reopened"} {
		ctx := map[string]any{"github": map[string]any{"event_name": "pull_request", "event": map[string]any{"action": action}}}
		for _, profile := range []string{"full", "portal-only", ""} {
			ran := simulateCIJobsWithProfile(t, w, ctx, profile)
			for _, id := range w.Jobs["required-ci"].Needs {
				want := profile == "full" || id == "scope" || id == "preflight" || id == "checks"
				if ran[id] != want {
					t.Errorf("%s/%s: job %s ran=%v, want %v", action, profile, id, ran[id], want)
				}
			}
			if !ran["required-ci"] {
				t.Errorf("%s/%s: aggregate must run even when dependencies skip", action, profile)
			}
		}
	}
	checks := w.Jobs["checks"]
	if checks.If != "" || checks.ContinueOnError {
		t.Fatal("portal checks must run unconditionally after preflight")
	}
	step := checks.step(t, "fmt · tidy · no-phone-home · vet · build · portal")
	if step.Run != "go run ./test/ci group checks" || step.If != "" || step.ContinueOnError {
		t.Fatal("both profiles must run the same complete portal check group")
	}
	all := groupChecksOnly(mergeGateChecks(), groupChecks)
	for _, label := range []string{
		"portal-build", "portal-audit", "portal-test", "portal-e2e",
		"portal-contract-generate", "portal-contract-diff", "portal-contract-test",
		"portal-embed-vet", "portal-embed-test", "portal-package", "portal-package-test",
	} {
		checkByLabel(t, all, label)
	}
}

func TestCIScopeUsesCompleteDiffAndDoesNotSavePartialCaches(t *testing.T) {
	t.Parallel()
	w := loadCIWorkflow(t)
	scope := w.Jobs["scope"]
	checkout := scope.stepUsing(t, "actions/checkout@")
	if checkout.with("fetch-depth") != "0" || checkout.with("ref") != "" {
		t.Fatal("scope must inspect the event checkout with complete git history")
	}
	setup := scope.stepUsing(t, "actions/setup-go@")
	if setup.with("cache") != "false" {
		t.Fatal("scope must not publish an incomplete shared Go build cache")
	}
	step := scope.step(t, "Classify validation scope")
	if step.Run != `go run ./test/cipolicy classify >> "$GITHUB_OUTPUT"` ||
		step.Env["CI_EVENT_NAME"] != "${{ github.event_name }}" ||
		step.Env["BASE_SHA"] != "${{ github.event.pull_request.base.sha }}" ||
		step.Env["HEAD_SHA"] != "${{ github.event.pull_request.head.sha }}" ||
		step.If != "" || step.ContinueOnError {
		t.Fatal("scope must invoke the classifier with exact event/base/head inputs")
	}
	gate := w.Jobs["required-ci"].step(t, "Verify required gates")
	if gate.Run != "go run ./test/cipolicy gate" || gate.Env["CI_NEEDS"] != "${{ toJSON(needs) }}" ||
		gate.Env["CI_EVENT_NAME"] != "${{ github.event_name }}" || gate.If != "" || gate.ContinueOnError {
		t.Fatal("aggregate must verify every dependency result with the event-aware policy")
	}
}

func hasStatusFunction(cond string) bool {
	for _, fn := range []string{"always()", "success()", "failure()", "cancelled()"} {
		if strings.Contains(cond, fn) {
			return true
		}
	}
	return false
}

// renderTemplate interpolates every ${{ }} in s.
func renderTemplate(t *testing.T, s string, ctx map[string]any) string {
	t.Helper()
	var out strings.Builder
	for {
		start := strings.Index(s, "${{")
		if start < 0 {
			return out.String() + s
		}
		end := strings.Index(s[start:], "}}")
		if end < 0 {
			t.Fatalf("unterminated expression in %q", s)
		}
		out.WriteString(s[:start])
		out.WriteString(toString(evalExpr(t, s[start+3:start+end], ctx, true)))
		s = s[start+end+2:]
	}
}

// evalExpr evaluates the subset of the GitHub Actions expression language
// ci.yml uses: literals, context paths, ! && || == != and parentheses, and the
// functions always, success, failure, cancelled and format.
func evalExpr(t *testing.T, src string, ctx map[string]any, success bool) any {
	t.Helper()
	p := &exprParser{src: src, ctx: ctx, success: success}
	v := p.or()
	if p.skipSpace(); p.pos != len(p.src) || p.err != nil {
		t.Fatalf("cannot evaluate %q at offset %d: %v", src, p.pos, p.err)
	}
	return v
}

type exprParser struct {
	src     string
	pos     int
	ctx     map[string]any
	success bool
	err     error
}

func (p *exprParser) skipSpace() {
	for p.pos < len(p.src) && p.src[p.pos] == ' ' {
		p.pos++
	}
}

func (p *exprParser) accept(tok string) bool {
	p.skipSpace()
	if strings.HasPrefix(p.src[p.pos:], tok) {
		p.pos += len(tok)
		return true
	}
	return false
}

func (p *exprParser) or() any {
	v := p.and()
	for p.accept("||") {
		rhs := p.and()
		if !truthy(v) {
			v = rhs
		}
	}
	return v
}

func (p *exprParser) and() any {
	v := p.equality()
	for p.accept("&&") {
		rhs := p.equality()
		if truthy(v) {
			v = rhs
		}
	}
	return v
}

func (p *exprParser) equality() any {
	v := p.unary()
	if p.accept("==") {
		return exprEqual(v, p.unary())
	}
	if p.accept("!=") {
		return !exprEqual(v, p.unary())
	}
	return v
}

func (p *exprParser) unary() any {
	if p.accept("!") {
		return !truthy(p.unary())
	}
	return p.primary()
}

func (p *exprParser) primary() any {
	p.skipSpace()
	switch {
	case p.accept("("):
		v := p.or()
		if !p.accept(")") {
			p.fail("missing )")
		}
		return v
	case p.accept("'"):
		end := strings.IndexByte(p.src[p.pos:], '\'')
		if end < 0 {
			p.fail("unterminated string")
			return nil
		}
		s := p.src[p.pos : p.pos+end]
		p.pos += end + 1
		return s
	}
	start := p.pos
	for p.pos < len(p.src) && strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-.", rune(p.src[p.pos])) {
		p.pos++
	}
	ident := p.src[start:p.pos]
	if p.accept("(") {
		return p.call(ident)
	}
	return p.lookup(ident)
}

func (p *exprParser) call(fn string) any {
	var args []any
	for !p.accept(")") {
		if len(args) > 0 && !p.accept(",") {
			p.fail("expected , in call")
			return nil
		}
		args = append(args, p.or())
		if p.err != nil {
			return nil
		}
	}
	switch fn {
	case "always":
		return true
	case "success":
		return p.success
	case "failure", "cancelled":
		return false
	case "format":
		s := toString(args[0])
		for i, a := range args[1:] {
			s = strings.ReplaceAll(s, fmt.Sprintf("{%d}", i), toString(a))
		}
		return s
	}
	p.fail("unsupported function " + fn)
	return nil
}

func (p *exprParser) lookup(path string) any {
	switch path {
	case "":
		p.fail("expected operand")
		return nil
	case "null":
		return nil
	case "true":
		return true
	case "false":
		return false
	}
	var v any = p.ctx
	for _, key := range strings.Split(path, ".") {
		m, _ := v.(map[string]any)
		v = m[key] // a missing property is null, as in Actions
	}
	return v
}

func (p *exprParser) fail(msg string) {
	if p.err == nil {
		p.err = fmt.Errorf("%s", msg)
	}
	p.pos = len(p.src)
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	}
	return true
}

func exprEqual(a, b any) bool {
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		return strings.EqualFold(as, bs)
	}
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ab, aok := a.(bool)
	bb, bok := b.(bool)
	return aok && bok && ab == bb
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
