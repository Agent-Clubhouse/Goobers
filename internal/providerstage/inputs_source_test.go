package providerstage

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type inputSourceOwner struct {
	command string
	// shared records inputs read by a helper in this source file on behalf of
	// additional commands. Every listed command must declare the field.
	shared map[string][]string
}

// providerInputSourceOwners is an audited call-site registry, not a duplicate
// field list. The AST below discovers field names directly from production
// providerInput calls and checks them against the command schema. A new source
// file must declare its command owner; a new call in an existing file is
// checked automatically.
var providerInputSourceOwners = map[string]inputSourceOwner{
	"applyverdict.go":           {command: "apply-verdict"},
	"backlogassignment.go":      {command: "backlog-assignment"},
	"backlogdedupe.go":          {command: "backlog-dedupe"},
	"backloghealth.go":          {command: "backlog-health"},
	"backlogquery.go":           {command: "backlog-query"},
	"backlogquery_policy.go":    {command: "backlog-query"},
	"backlogreport.go":          {command: "backlog-query"},
	"backlogresweep.go":         {command: "backlog-query"},
	"backlogstaleness.go":       {command: "backlog-query"},
	"cancelpendingci.go":        {command: "cancel-pending-ci"},
	"checkfailfirst.go":         {command: "check-fail-first"},
	"checkissuestaleness.go":    {command: "check-issue-staleness"},
	"demo_provider.go":          {command: "__demo-provider"},
	"docchurn.go":               {command: "docs-churn"},
	"electlander.go":            {command: "elect-lander"},
	"fileissues.go":             {command: "file-issues"},
	"gateremovalguard.go":       {command: "gate-removal-guard"},
	"gathercifailures.go":       {command: "gather-ci-failures"},
	"gatherissuecontext.go":     {command: "gather-issue-context"},
	"gatherprcontext.go":        {command: "gather-pr-context", shared: map[string][]string{"minSeverity": {"gather-sibling-context", "update-behind-pr"}}},
	"gatherreviewthreads.go":    {command: "gather-review-threads"},
	"implementcontext.go":       {command: "gather-implement-context"},
	"iossimulator.go":           {command: "ios-simulator-test"},
	"issuecloseout.go":          {command: "issue-close-out"},
	"mergepr.go":                {command: "merge-pr"},
	"mergequeuepoll.go":         {command: "merge-queue-poll"},
	"openpr.go":                 {command: "open-pr"},
	"postmerge.go":              {command: "post-merge"},
	"postmergereconcile.go":     {command: "reconcile-post-merge"},
	"prclaim.go":                {command: "pr-claim"},
	"prcommentwatch.go":         {command: "pr-comment-watch"},
	"preflightrepowrite.go":     {command: "preflight-repo-write"},
	"prqueuereport.go":          {command: "backlog-query"},
	"prremediationlifecycle.go": {command: "pr-claim"},
	"prselect.go":               {command: "pr-select"},
	"prsiblingcontext.go":       {command: "gather-sibling-context"},
	"publishbatch.go":           {command: "publish-batch"},
	"pushremediated.go":         {command: "push-remediated"},
	"rebasepr.go":               {command: "rebase-pr"},
	"reconcilebranches.go":      {command: "reconcile-branches"},
	"recordmergerefusal.go":     {command: "record-merge-refusal"},
	"remediationcheckpoint.go":  {command: "remediation-checkpoint"},
	"reportprstatus.go":         {command: "report-pr-status"},
	"resolvereviewthreads.go":   {command: "resolve-review-threads"},
	"respondtofindings.go":      {command: "respond-to-findings"},
	"securityalerts.go":         {command: "security-alerts-query"},
	"selectsource.go":           {command: "select-source"},
	"selfupdate.go":             {command: "self-update"},
	"setmilestone.go":           {command: "set-milestone"},
	"telemetryquery.go":         {command: "telemetry-query"},
	"updatebehindpr.go":         {command: "update-behind-pr"},
	"validateplan.go":           {command: "validate-plan"},

	// Generic provider-stage plumbing reads timeout and resultFile for several
	// commands. Its fields are checked against the registry union below.
	"providercmd.go": {},
}

func TestProviderInputConsumersMatchSchemas(t *testing.T) {
	directory := filepath.Join("..", "..", "cmd", "goobers")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		calls, err := providerInputCalls(name, data)
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) == 0 {
			continue
		}
		owner, ok := providerInputSourceOwners[name]
		if !ok {
			t.Errorf("%s reads provider inputs but has no audited command owner", name)
			continue
		}
		seen[name] = true
		for _, call := range calls {
			for _, problem := range providerInputCallProblems(owner, call) {
				t.Errorf("%s:%d: %s", name, call.line, problem)
			}
		}
	}
	for name := range providerInputSourceOwners {
		if !seen[name] {
			t.Errorf("audited provider-input source %s no longer contains a providerInput call", name)
		}
	}
}

func TestProviderInputConsumerAuditRejectsUndeclaredField(t *testing.T) {
	for _, field := range []string{"newKey", "base"} {
		t.Run(field, func(t *testing.T) {
			source := fmt.Sprintf("package main\nfunc run() { _ = providerInput(%q, %q) }\n", field, "")
			calls, err := providerInputCalls("mutation.go", []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			problems := providerInputCallProblems(inputSourceOwner{command: "self-update"}, calls[0])
			want := fmt.Sprintf(`self-update schema does not declare provider input %q`, field)
			if len(problems) != 1 || !strings.Contains(problems[0], want) {
				t.Fatalf("problems = %q, want field-level schema drift refusal %q", problems, want)
			}
		})
	}
}

type providerInputCall struct {
	name string
	line int
}

func providerInputCalls(filename string, source []byte) ([]providerInputCall, error) {
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, filename, source, 0)
	if err != nil {
		return nil, err
	}
	var calls []providerInputCall
	var inspectErr error
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		function, ok := call.Fun.(*ast.Ident)
		if !ok || (function.Name != "providerInput" && function.Name != "providerInputLookup") {
			return true
		}
		name, ok := providerInputArgumentName(call.Args[0])
		if !ok {
			inspectErr = fmt.Errorf("%s:%d: providerInput name must be a string literal or audited executor input constant", filename, files.Position(call.Pos()).Line)
			return true
		}
		calls = append(calls, providerInputCall{name: name, line: files.Position(call.Pos()).Line})
		return true
	})
	return calls, inspectErr
}

func providerInputArgumentName(expression ast.Expr) (string, bool) {
	switch value := expression.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "", false
		}
		name, err := strconv.Unquote(value.Value)
		return name, err == nil
	case *ast.SelectorExpr:
		packageName, ok := value.X.(*ast.Ident)
		if !ok || packageName.Name != "executor" {
			return "", false
		}
		switch value.Sel.Name {
		case "InputTimeout":
			return "timeout", true
		case "InputResultFile":
			return "resultFile", true
		case "InputMaxOutputBytes":
			return "maxOutputBytes", true
		}
	}
	return "", false
}

func providerInputCallProblems(owner inputSourceOwner, call providerInputCall) []string {
	if owner.command == "" {
		for command := range inputSchemas {
			if schemaHasInput(command, call.name) {
				return nil
			}
		}
		return []string{fmt.Sprintf("generic provider plumbing reads undeclared input %q", call.name)}
	}
	commands := append([]string{owner.command}, owner.shared[call.name]...)
	var problems []string
	for _, command := range commands {
		if !schemaHasInput(command, call.name) {
			problems = append(problems, fmt.Sprintf("%s schema does not declare provider input %q", command, call.name))
		}
	}
	return problems
}

func schemaHasInput(command, name string) bool {
	declared, known := inputSchemas[command]
	return known && (slices.ContainsFunc(declared, func(input Input) bool { return input.Name == name }) ||
		slices.ContainsFunc(executorInputs, func(input Input) bool { return input.Name == name }))
}
