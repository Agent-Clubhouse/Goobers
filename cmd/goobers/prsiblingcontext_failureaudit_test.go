package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
)

var gatherSiblingFailConsumerFields = []string{
	"advisoryMode",
	"overlappingSiblingsCsv",
	"reviewDigest",
	"scopeGateParked",
	"selectedBaseSha",
	"selectedHeadSha",
	"selectedNumber",
}

func TestGatherSiblingContextShippedFailBranchConsumers(t *testing.T) {
	configRoots := []string{
		filepath.Join("..", "..", "reference-workflows"),
		filepath.Join("..", "..", "config-examples"),
		filepath.Join("..", "..", "examples", "ios-simulator"),
		filepath.Join("..", "..", "internal", "instance", "starter"),
		filepath.Join("..", "..", "internal", "instance", "quickstart-v1"),
	}

	producers := 0
	var consumers []string
	for _, root := range configRoots {
		set, report, err := instance.LoadConfigDir(root)
		if err != nil {
			t.Fatalf("load shipped config %s: %v\n%v", root, err, report)
		}
		for _, definition := range set.Workflows {
			tasks := make(map[string]apiv1.Task, len(definition.Spec.Tasks))
			for _, task := range definition.Spec.Tasks {
				tasks[task.Name] = task
			}
			gates := make(map[string]apiv1.Gate, len(definition.Spec.Gates))
			for _, gate := range definition.Spec.Gates {
				gates[gate.Name] = gate
			}
			for _, producer := range definition.Spec.Tasks {
				if !invokesGatherSiblingContext(producer) {
					continue
				}
				producers++
				walkGatherSiblingGatePaths(
					t,
					definition.Spec.Gaggle+"/"+definition.Name,
					producer,
					producer.Next,
					[]string{producer.Name},
					false,
					tasks,
					gates,
					map[string]bool{},
					&consumers,
				)
			}
		}
	}

	sort.Strings(consumers)
	want := []string{
		"acme-web-claude/claude-merge-review: gather-sibling-context -> review[fail] -> apply-verdict inputsFrom={advisoryMode=advisoryMode,overlappingSiblings=overlappingSiblingsCsv,reviewDigest=reviewDigest,scopeGateParked=scopeGateParked,selectedBaseSha=selectedBaseSha,selectedHeadSha=selectedHeadSha,selectedNumber=selectedNumber}",
		"acme-web/merge-review: gather-sibling-context -> review[fail] -> apply-verdict inputsFrom={advisoryMode=advisoryMode,overlappingSiblings=overlappingSiblingsCsv,reviewDigest=reviewDigest,scopeGateParked=scopeGateParked,selectedBaseSha=selectedBaseSha,selectedHeadSha=selectedHeadSha,selectedNumber=selectedNumber}",
		"goobers/merge-review: gather-sibling-context -> review[fail] -> apply-verdict inputsFrom={advisoryMode=advisoryMode,overlappingSiblings=overlappingSiblingsCsv,reviewDigest=reviewDigest,scopeGateParked=scopeGateParked,selectedBaseSha=selectedBaseSha,selectedHeadSha=selectedHeadSha,selectedNumber=selectedNumber}",
	}
	if producers != 4 {
		t.Fatalf("found %d shipped gather-sibling-context stages, want audited inventory of 4", producers)
	}
	if !reflect.DeepEqual(consumers, want) {
		t.Fatalf("gather-sibling-context fail-branch consumers =\n%s\nwant\n%s",
			strings.Join(consumers, "\n"), strings.Join(want, "\n"))
	}
}

func walkGatherSiblingGatePaths(
	t *testing.T,
	workflowName string,
	producer apiv1.Task,
	target string,
	trail []string,
	viaFail bool,
	tasks map[string]apiv1.Task,
	gates map[string]apiv1.Gate,
	stack map[string]bool,
	consumers *[]string,
) {
	t.Helper()
	if target == "" {
		return
	}
	if consumer, ok := tasks[target]; ok {
		if !viaFail {
			return
		}
		handoffs := make([]string, 0, len(consumer.InputsFrom))
		for input, output := range consumer.InputsFrom {
			handoffs = append(handoffs, input+"="+output)
		}
		sort.Strings(handoffs)
		*consumers = append(*consumers, fmt.Sprintf(
			"%s: %s -> %s inputsFrom={%s}",
			workflowName,
			strings.Join(trail, " -> "),
			consumer.Name,
			strings.Join(handoffs, ","),
		))
		return
	}
	gate, ok := gates[target]
	if !ok {
		t.Fatalf("%s: %s routes to unknown target %q", workflowName, producer.Name, target)
	}
	if stack[gate.Name] {
		t.Fatalf("%s: gate-only cycle after %s reaches %s", workflowName, producer.Name, gate.Name)
	}
	stack[gate.Name] = true
	defer delete(stack, gate.Name)

	outcomes := make([]string, 0, len(gate.Branches))
	for outcome := range gate.Branches {
		outcomes = append(outcomes, outcome)
	}
	sort.Strings(outcomes)
	for _, outcome := range outcomes {
		walkGatherSiblingGatePaths(
			t,
			workflowName,
			producer,
			gate.Branches[outcome],
			append(trail, gate.Name+"["+outcome+"]"),
			viaFail || outcome == "fail",
			tasks,
			gates,
			stack,
			consumers,
		)
	}
}

func invokesGatherSiblingContext(task apiv1.Task) bool {
	if task.Run == nil {
		return false
	}
	for index, argument := range task.Run.Command {
		if argument == "gather-sibling-context" && index > 0 && filepath.Base(task.Run.Command[index-1]) == "goobers" {
			return true
		}
	}
	return false
}

func TestGatherSiblingContextFatalProviderPathInventory(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve audit test path")
	}
	path := filepath.Join(filepath.Dir(testFile), "prsiblingcontext.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	got := map[string]int{}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		function, ok := call.Fun.(*ast.Ident)
		if !ok || function.Name != "failProviderStage" || len(call.Args) < 2 {
			return true
		}
		name, ok := providerFailureOperation(call.Args[1])
		if !ok {
			t.Fatalf("failProviderStage in %s must use a literal operation name or fmt.Sprintf format", path)
		}
		got[name]++
		return true
	})

	want := map[string]int{
		"check state for PR #%d":              1,
		"list comments on PR #%d":             1,
		"list files for PR #%d":               1,
		"list files for selected PR #%d":      1,
		"list pull requests":                  1,
		"resolve merge-review verdict author": 1,
		// The Azure DevOps branch (runGatherSiblingContextADO) resolves the
		// selected PR's deterministic pin with a single PollPullRequest; its
		// failure envelope is covered by
		// TestGatherSiblingContextADOPollFailureKeepsGenericEnvelope.
		"poll pull request #%s": 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fatal provider path inventory = %v, want %v; add fault-injection coverage for any new path", got, want)
	}
}

func providerFailureOperation(expression ast.Expr) (string, bool) {
	switch expression := expression.(type) {
	case *ast.BasicLit:
		if expression.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(expression.Value)
		return value, err == nil
	case *ast.CallExpr:
		selector, ok := expression.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Sprintf" || len(expression.Args) == 0 {
			return "", false
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "fmt" {
			return "", false
		}
		return providerFailureOperation(expression.Args[0])
	default:
		return "", false
	}
}

func TestGatherSiblingContextFatalProviderPathsKeepGenericEnvelope(t *testing.T) {
	prefix := "/repos/your-org/your-repo"
	tests := []struct {
		name      string
		operation string
		match     func(*http.Request) bool
	}{
		{
			name:      "pull request listing",
			operation: "list pull requests",
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet && r.URL.Path == prefix+"/pulls"
			},
		},
		{
			name:      "selected pull request files",
			operation: "list files for selected PR #10",
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet && r.URL.Path == prefix+"/pulls/10/files"
			},
		},
		{
			name:      "sibling pull request files",
			operation: "list files for PR #11",
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet && r.URL.Path == prefix+"/pulls/11/files"
			},
		},
		{
			name:      "sibling check state combined status",
			operation: "check state for PR #11",
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet && r.URL.Path == prefix+"/commits/sha11/status"
			},
		},
		{
			// RefCheckState resolves a ref from two sequential paged requests:
			// the legacy combined status above, then check-runs. Injecting into
			// the first never exercises the second, because the first error
			// returns before it is issued — so this case is the only coverage
			// of a failure after the combined status already succeeded.
			name:      "sibling check state check runs",
			operation: "check state for PR #11",
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet && r.URL.Path == prefix+"/commits/sha11/check-runs"
			},
		},
		{
			name:      "selected pull request comments",
			operation: "list comments on PR #10",
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet && r.URL.Path == prefix+"/issues/10/comments"
			},
		},
		{
			name:      "authenticated login",
			operation: "resolve merge-review verdict author",
			match: func(r *http.Request) bool {
				return r.Method == http.MethodGet && r.URL.Path == "/user"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(10, "Selected PR")
			server.addOpenPR(10, "goobers/implementation/run-10", "main", "sha10", "base",
				false, nil, []fakePRFile{{path: "shared.go", status: "modified"}})
			server.addIssue(11, "Sibling PR")
			server.addOpenPR(11, "goobers/implementation/run-11", "main", "sha11", "base",
				false, nil, []fakePRFile{{path: "shared.go", status: "modified"}})
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1764-provider-error")
			t.Setenv(executor.InputEnvVar("selectedNumber"), "10")
			t.Setenv(executor.InputEnvVar("hasSubstantiveFindings"), "true")

			baseHandler := server.server.Config.Handler
			var injected atomic.Bool
			server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !injected.Load() && test.match(r) {
					injected.Store(true)
					http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
					return
				}
				baseHandler.ServeHTTP(w, r)
			})

			workDir := t.TempDir()
			t.Chdir(workDir)
			code, _, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root)
			if code != 1 {
				t.Fatalf("%s under a 422: code = %d, stderr = %q, want 1", test.operation, code, stderr)
			}
			if !injected.Load() {
				t.Fatalf("%s did not reach the audited provider request", test.operation)
			}
			result := readProviderStageResult(t, filepath.Join(workDir, "sibling-context.json"))
			if result[executor.OutputErrorCode] != errorCodeProvider {
				t.Fatalf("errorCode = %v, want %s", result[executor.OutputErrorCode], errorCodeProvider)
			}
			if result[executor.OutputErrorRetryable] != false {
				t.Fatalf("errorRetryable = %v, want false", result[executor.OutputErrorRetryable])
			}
			message, _ := result[executor.OutputErrorMessage].(string)
			if !strings.Contains(message, test.operation) || !strings.Contains(message, "status 422") {
				t.Fatalf("errorMessage = %q, want operation %q and provider status", message, test.operation)
			}
			if result["integrity"] != string(apiv1.IntegrityUnapproved) {
				t.Fatalf("sibling-context.json integrity = %v, want unapproved", result["integrity"])
			}
			if len(result) != 4 {
				t.Fatalf("sibling-context.json = %v, want only the generic provider failure envelope and integrity", result)
			}
			for _, field := range gatherSiblingFailConsumerFields {
				if _, ok := result[field]; ok {
					t.Fatalf("sibling-context.json = %v, unclassified provider failure must not synthesize %s", result, field)
				}
			}
		})
	}
}

func TestGatherSiblingContextClassifiesSiblingLifecycleAfterCheckFailure(t *testing.T) {
	tests := []struct {
		name        string
		outcome     string
		mutate      func(*fakePR)
		wantSibling bool
		wantHeadSHA string
	}{
		{
			name:        "closed",
			outcome:     "closed",
			wantHeadSHA: "sha11",
			mutate: func(pr *fakePR) {
				pr.state = "closed"
			},
		},
		{
			name:        "merged",
			outcome:     "merged",
			wantHeadSHA: "sha11",
			mutate: func(pr *fakePR) {
				pr.state = "closed"
				pr.merged = true
			},
		},
		{
			name:    "head moved",
			outcome: "head-moved",
			mutate: func(pr *fakePR) {
				pr.headSHA = "sha11-next"
			},
			wantSibling: true,
			wantHeadSHA: "sha11-next",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(10, "Selected PR")
			server.addOpenPR(10, "goobers/implementation/run-10", "main", "sha10", "base",
				false, nil, []fakePRFile{{path: "selected.go", status: "modified"}})
			server.addIssue(11, "Sibling PR")
			server.addOpenPR(11, "goobers/implementation/run-11", "main", "sha11", "base",
				false, nil, []fakePRFile{{path: "sibling.go", status: "modified"}})
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1866-lifecycle")
			t.Setenv(executor.InputEnvVar("selectedNumber"), "10")

			baseHandler := server.server.Config.Handler
			var injected atomic.Bool
			server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet &&
					r.URL.Path == "/repos/your-org/your-repo/commits/sha11/status" &&
					injected.CompareAndSwap(false, true) {
					server.mu.Lock()
					test.mutate(server.prs[11])
					server.mu.Unlock()
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
					return
				}
				baseHandler.ServeHTTP(w, r)
			})

			workDir := t.TempDir()
			t.Chdir(workDir)
			if code, _, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root); code != 0 {
				t.Fatalf("gather-sibling-context: code = %d, stderr = %q", code, stderr)
			}
			if !injected.Load() {
				t.Fatal("did not inject the sibling check-state failure")
			}

			result := readProviderStageResult(t, filepath.Join(workDir, "sibling-context.json"))
			outcomes, ok := result["siblingLifecycleOutcomes"].([]interface{})
			if !ok || len(outcomes) != 1 {
				t.Fatalf("siblingLifecycleOutcomes = %T(%v), want one outcome", result["siblingLifecycleOutcomes"], result["siblingLifecycleOutcomes"])
			}
			outcome, ok := outcomes[0].(map[string]interface{})
			if !ok {
				t.Fatalf("siblingLifecycleOutcomes[0] = %T(%v), want object", outcomes[0], outcomes[0])
			}
			if outcome["number"] != float64(11) || outcome["outcome"] != test.outcome ||
				outcome["previousHeadSha"] != "sha11" || outcome["currentHeadSha"] != test.wantHeadSHA {
				t.Fatalf("siblingLifecycleOutcomes[0] = %v, want PR #11 %s from sha11 to %q",
					outcome, test.outcome, test.wantHeadSHA)
			}

			siblings, ok := result["siblings"].([]interface{})
			if !ok {
				t.Fatalf("siblings = %T(%v), want array", result["siblings"], result["siblings"])
			}
			if test.wantSibling {
				if len(siblings) != 1 || siblings[0].(map[string]interface{})["headSha"] != test.wantHeadSHA {
					t.Fatalf("siblings = %v, want refreshed sibling at %s", siblings, test.wantHeadSHA)
				}
			} else if len(siblings) != 0 {
				t.Fatalf("siblings = %v, want terminal sibling omitted", siblings)
			}
		})
	}
}

func TestGatherSiblingContextRefreshesBeforeClassifyingHeadMoveRetryFailure(t *testing.T) {
	tests := []struct {
		name           string
		mutateOnRetry  func(*fakePR)
		wantOutcome    string
		wantCurrentSHA string
	}{
		{
			name: "closed during retry",
			mutateOnRetry: func(pr *fakePR) {
				pr.state = "closed"
			},
			wantOutcome:    "closed",
			wantCurrentSHA: "sha11-next",
		},
		{
			name: "head moved again during retry",
			mutateOnRetry: func(pr *fakePR) {
				pr.headSHA = "sha11-final"
			},
			wantOutcome:    "head-moved",
			wantCurrentSHA: "sha11-final",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			server.addIssue(10, "Selected PR")
			server.addOpenPR(10, "goobers/implementation/run-10", "main", "sha10", "base",
				false, nil, []fakePRFile{{path: "selected.go", status: "modified"}})
			server.addIssue(11, "Sibling PR")
			server.addOpenPR(11, "goobers/implementation/run-11", "main", "sha11", "base",
				false, nil, []fakePRFile{{path: "sibling.go", status: "modified"}})
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1866-retry-lifecycle")
			t.Setenv(executor.InputEnvVar("selectedNumber"), "10")

			baseHandler := server.server.Config.Handler
			var checkFailures atomic.Int32
			server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/commits/sha11/status" {
					server.mu.Lock()
					server.prs[11].headSHA = "sha11-next"
					server.mu.Unlock()
					checkFailures.Add(1)
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == "/repos/your-org/your-repo/commits/sha11-next/status" {
					server.mu.Lock()
					test.mutateOnRetry(server.prs[11])
					server.mu.Unlock()
					checkFailures.Add(1)
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
					return
				}
				baseHandler.ServeHTTP(w, r)
			})

			workDir := t.TempDir()
			t.Chdir(workDir)
			if code, _, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root); code != 0 {
				t.Fatalf("gather-sibling-context: code = %d, stderr = %q", code, stderr)
			}
			if got := checkFailures.Load(); got != 2 {
				t.Fatalf("check-state failures = %d, want 2 bounded attempts", got)
			}

			result := readProviderStageResult(t, filepath.Join(workDir, "sibling-context.json"))
			outcomes, ok := result["siblingLifecycleOutcomes"].([]interface{})
			if !ok || len(outcomes) != 2 {
				t.Fatalf("siblingLifecycleOutcomes = %T(%v), want two outcomes", result["siblingLifecycleOutcomes"], result["siblingLifecycleOutcomes"])
			}
			retryOutcome, ok := outcomes[1].(map[string]interface{})
			if !ok {
				t.Fatalf("siblingLifecycleOutcomes[1] = %T(%v), want object", outcomes[1], outcomes[1])
			}
			if retryOutcome["number"] != float64(11) || retryOutcome["outcome"] != test.wantOutcome ||
				retryOutcome["previousHeadSha"] != "sha11-next" || retryOutcome["currentHeadSha"] != test.wantCurrentSHA {
				t.Fatalf("siblingLifecycleOutcomes[1] = %v, want PR #11 %s from sha11-next to %q",
					retryOutcome, test.wantOutcome, test.wantCurrentSHA)
			}
			siblings, ok := result["siblings"].([]interface{})
			if !ok || len(siblings) != 0 {
				t.Fatalf("siblings = %T(%v), want retry-raced sibling omitted", result["siblings"], result["siblings"])
			}
		})
	}
}

func TestGatherSiblingContextFailedLifecycleRefreshKeepsGenericEnvelope(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(10, "Selected PR")
	server.addOpenPR(10, "goobers/implementation/run-10", "main", "sha10", "base",
		false, nil, []fakePRFile{{path: "selected.go", status: "modified"}})
	server.addIssue(11, "Sibling PR")
	server.addOpenPR(11, "goobers/implementation/run-11", "main", "sha11", "base",
		false, nil, []fakePRFile{{path: "sibling.go", status: "modified"}})
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1866-refresh-failure")
	t.Setenv(executor.InputEnvVar("selectedNumber"), "10")

	baseHandler := server.server.Config.Handler
	var checkFailed atomic.Bool
	server.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet &&
			r.URL.Path == "/repos/your-org/your-repo/commits/sha11/status" &&
			checkFailed.CompareAndSwap(false, true):
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case checkFailed.Load() && r.Method == http.MethodGet &&
			r.URL.Path == "/repos/your-org/your-repo/pulls/11":
			http.Error(w, `{"message":"Service Unavailable"}`, http.StatusServiceUnavailable)
		default:
			baseHandler.ServeHTTP(w, r)
		}
	})

	workDir := t.TempDir()
	t.Chdir(workDir)
	code, _, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root)
	if code != 1 {
		t.Fatalf("gather-sibling-context: code = %d, stderr = %q, want 1", code, stderr)
	}
	result := readProviderStageResult(t, filepath.Join(workDir, "sibling-context.json"))
	if result[executor.OutputErrorCode] != errorCodeProvider || len(result) != 4 {
		t.Fatalf("sibling-context.json = %v, want generic provider failure envelope", result)
	}
	if _, ok := result["siblingLifecycleOutcomes"]; ok {
		t.Fatalf("sibling-context.json = %v, failed refresh must not classify a lifecycle outcome", result)
	}
}

func TestGatherSiblingContextSuccessIncludesFailConsumerFields(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	server.addIssue(10, "Selected PR")
	server.addOpenPR(10, "goobers/implementation/run-10", "main", "sha10", "base",
		false, nil, []fakePRFile{{path: "selected.go", status: "modified"}})
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1764-success")
	t.Setenv(executor.InputEnvVar("selectedNumber"), "10")

	workDir := t.TempDir()
	t.Chdir(workDir)
	if code, _, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root); code != 0 {
		t.Fatalf("gather-sibling-context: code = %d, stderr = %q", code, stderr)
	}

	result := readProviderStageResult(t, filepath.Join(workDir, "sibling-context.json"))
	for _, field := range gatherSiblingFailConsumerFields {
		value, ok := result[field]
		if !ok {
			t.Fatalf("sibling-context.json = %v, missing fail-branch consumer field %s", result, field)
		}
		if _, ok := value.(string); !ok {
			t.Fatalf("sibling-context.json[%s] = %T(%v), want string for inputsFrom", field, value, value)
		}
	}
}

func TestGatherSiblingContextTerminalBusinessOutcomesAreNoWork(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeGitHubServer)
	}{
		{
			name:  "selected pull request is no longer open",
			setup: func(*fakeGitHubServer) {},
		},
		{
			name: "selected pull request opted out",
			setup: func(server *fakeGitHubServer) {
				server.addIssue(10, "Opted-out PR")
				server.addOpenPR(10, "goobers/implementation/run-10", "main", "sha10", "base",
					false, []string{noMergeReviewLabel}, nil)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := initDemo(t)
			server := newFakeGitHubServer(t, "your-org", "your-repo")
			test.setup(server)
			providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1764-no-work")
			t.Setenv(executor.InputEnvVar("selectedNumber"), "10")

			workDir := t.TempDir()
			t.Chdir(workDir)
			if code, _, stderr := runArgs(t, "gather-sibling-context", "--no-verdict-cache", root); code != 0 {
				t.Fatalf("gather-sibling-context: code = %d, stderr = %q", code, stderr)
			}

			result := readProviderStageResult(t, filepath.Join(workDir, "sibling-context.json"))
			want := map[string]interface{}{
				"claimed":             false,
				executor.OutputNoWork: true,
				"integrity":           string(apiv1.IntegrityUnapproved),
			}
			if !reflect.DeepEqual(result, want) {
				t.Fatalf("sibling-context.json = %v, want terminal no-work result %v", result, want)
			}
		})
	}
}
