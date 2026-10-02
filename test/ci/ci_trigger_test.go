package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// ciMetadataEdit is the one predicate ci.yml uses to recognise a pull-request
// title/body edit: an `edited` event that does not retarget the base branch.
// Spelled identically everywhere it appears so the workflow cannot disagree
// with itself about which edits re-run CI (#6360, #5491).
const ciMetadataEdit = "(github.event.action == 'edited' && !github.event.changes.base)"

// ciCodeGate is the `if:` on ci.yml's root jobs (preflight, unit). Every other
// validation job needs one of them, so a false gate skips the whole run.
const ciCodeGate = "${{ !" + ciMetadataEdit + " }}"

const pinnedRequiredCheck = "make ci (fmt-check · vet · build · test · lint)"

// ciEventFixture is one GitHub event as ci.yml's expressions see it.
type ciEventFixture struct {
	name  string
	event string
	// payload is github.event: only the fields the workflow reads.
	payload map[string]any
	// validates: the run executes the full required gate under the pinned
	// required-check name, in the ref's shared (cancelling) concurrency group.
	// When false the run must schedule no job, cancel nothing, and render a
	// required-ci name that cannot stand in for the pinned check.
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
		{"base retarget", "pull_request", pr("edited", map[string]any{"base": base}), true},
		{"base retarget with title edit", "pull_request", pr("edited", map[string]any{"base": base, "title": from("old")}), true},
		{"merge queue", "merge_group", pr("checks_requested", nil), true},
		{"manual dispatch", "workflow_dispatch", map[string]any{}, true},
	}
}

// TestCIRunsOnBaseRetargetNotMetadataEdits evaluates ci.yml's own trigger,
// concurrency, job conditions and required-check name against event fixtures
// (#6360, carried from #5491). A base retarget changes the merge ref, so it
// must re-run the full gate and replace the stale run. A title/body edit
// changes no code: it must not cancel the in-flight run (own concurrency
// group), must schedule no job, and must not publish a skipped check under the
// pinned required name — a skipped required check satisfies the ruleset, so it
// would let a red PR through instead of failing closed.
func TestCIRunsOnBaseRetargetNotMetadataEdits(t *testing.T) {
	t.Parallel()
	w := loadCIWorkflow(t)
	var types []string
	trigger := w.On["pull_request"]
	if err := trigger.Decode(&struct {
		Types *[]string `yaml:"types"`
	}{&types}); err != nil {
		t.Fatalf("decode pull_request trigger: %v", err)
	}
	if !slices.Contains(types, "edited") {
		t.Fatalf("pull_request types %v must include edited so a base retarget re-runs CI", types)
	}
	for _, root := range []string{"preflight", "unit"} {
		if got := w.Jobs[root].If; got != ciCodeGate {
			t.Errorf("root job %s if = %q, want %q", root, got, ciCodeGate)
		}
	}

	const runID = "4242"
	for _, fx := range ciEventFixtures() {
		t.Run(fx.name, func(t *testing.T) {
			ctx := map[string]any{"github": map[string]any{
				"event_name": fx.event, "event": fx.payload, "workflow": "CI",
				"ref": "refs/pull/1/merge", "run_id": runID,
			}}
			group := renderTemplate(t, w.Concurrency.Group, ctx)
			shared := "ci-CI-refs/pull/1/merge"
			if fx.validates && group != shared {
				t.Errorf("concurrency group = %q, want the ref's shared group %q so it replaces the stale run", group, shared)
			}
			if !fx.validates && (group == shared || !strings.Contains(group, runID)) {
				t.Errorf("concurrency group = %q; a metadata edit needs a run-unique group so it cancels nothing", group)
			}

			ran := simulateCIJobs(t, w, ctx)
			name := renderTemplate(t, w.Jobs["required-ci"].Name, ctx)
			if fx.validates {
				for _, id := range append([]string{"required-ci"}, w.Jobs["required-ci"].Needs...) {
					if !ran[id] {
						t.Errorf("required job %s does not run", id)
					}
				}
				if name != pinnedRequiredCheck {
					t.Errorf("required-ci renders %q, want the ruleset-pinned %q", name, pinnedRequiredCheck)
				}
				return
			}
			for id, r := range ran {
				if r {
					t.Errorf("job %s runs on a title/body edit; it must schedule nothing", id)
				}
			}
			if name == pinnedRequiredCheck {
				t.Error("required-ci keeps the pinned name on a title/body edit; its skipped check would shadow the real result")
			}
		})
	}
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
			needs[dep] = map[string]any{"result": result, "outputs": map[string]any{}}
		}
		cond := strings.TrimSpace(job.If)
		cond = strings.TrimSuffix(strings.TrimPrefix(cond, "${{"), "}}")
		if strings.TrimSpace(cond) == "" {
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
