package gate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/executor"
	wf "github.com/goobers/goobers/internal/workflow"
)

func evalCheck(t *testing.T, check string, params map[string]string, inputs map[string]interface{}) (string, error) {
	t.Helper()
	e := NewAutomatedEvaluator()
	env := apiv1.InvocationEnvelope{Inputs: inputs}
	return e.Evaluate(context.Background(), apiv1.AutomatedGate{Check: check, Params: params}, env)
}

func TestAcmeClaudeAdvisoryVerdictRoutesBothModes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config-examples", "gaggles", "acme-web-claude", "workflows", "merge-review.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var definition apiv1.Workflow
	if err := yaml.Unmarshal(raw, &definition); err != nil {
		t.Fatal(err)
	}

	var selector, publisher apiv1.Task
	for _, task := range definition.Spec.Tasks {
		switch task.Name {
		case "pr-select":
			selector = task
		case "apply-verdict":
			publisher = task
		}
	}
	if !containsString(selector.ExpectedOutputs, "advisoryMode") {
		t.Fatal("pr-select does not declare advisoryMode as an expected output")
	}
	if got := publisher.InputsFrom["advisoryMode"]; got != "advisoryMode" {
		t.Fatalf("apply-verdict advisoryMode input source = %q, want advisoryMode", got)
	}
	if !containsString(publisher.ExpectedOutputs, "advisoryMode") {
		t.Fatal("apply-verdict does not declare advisoryMode as an expected output")
	}

	var configured apiv1.Gate
	for _, candidate := range definition.Spec.Gates {
		if candidate.Name == "advisory-verdict" {
			configured = candidate
			break
		}
	}
	if configured.Automated == nil {
		t.Fatal("advisory-verdict automated gate not found")
	}

	for _, test := range []struct {
		mode        string
		wantOutcome string
		wantTarget  string
	}{
		{mode: "true", wantOutcome: OutcomePass, wantTarget: wf.TerminalComplete},
		{mode: "false", wantOutcome: OutcomeFail, wantTarget: "published-verdict"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			outcome, err := NewAutomatedEvaluator().Evaluate(
				context.Background(),
				*configured.Automated,
				apiv1.InvocationEnvelope{Inputs: map[string]interface{}{"advisoryMode": test.mode}},
			)
			if err != nil {
				t.Fatal(err)
			}
			if outcome != test.wantOutcome {
				t.Fatalf("outcome = %q, want %q", outcome, test.wantOutcome)
			}
			if target, ok := configured.Branches[outcome]; !ok || target != test.wantTarget {
				t.Fatalf("branch target = %q, %t; want %q, true", target, ok, test.wantTarget)
			}
		})
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestStatusEqualsDefaultsToSuccess(t *testing.T) {
	out, err := evalCheck(t, "status-equals", nil, map[string]interface{}{InputKeyStatus: "success"})
	if err != nil || out != OutcomePass {
		t.Fatalf("got %q, %v; want pass", out, err)
	}
	out, err = evalCheck(t, "status-equals", nil, map[string]interface{}{InputKeyStatus: "failure"})
	if err != nil || out != OutcomeFail {
		t.Fatalf("got %q, %v; want fail", out, err)
	}
}

func TestStatusEqualsCustomTarget(t *testing.T) {
	out, err := evalCheck(t, "status-equals", map[string]string{"equals": "blocked"}, map[string]interface{}{InputKeyStatus: "blocked"})
	if err != nil || out != OutcomePass {
		t.Fatalf("got %q, %v; want pass", out, err)
	}
}

func TestCompiledRegexCacheIsBounded(t *testing.T) {
	regexCacheMutex.Lock()
	oldCache := regexCache
	oldOrder := regexCacheOrder
	regexCache = make(map[string]*regexp.Regexp, maxRegexCacheEntries)
	regexCacheOrder = nil
	regexCacheMutex.Unlock()
	t.Cleanup(func() {
		regexCacheMutex.Lock()
		regexCache = oldCache
		regexCacheOrder = oldOrder
		regexCacheMutex.Unlock()
	})

	for i := 0; i < maxRegexCacheEntries+20; i++ {
		_, err := getCompiledRegex(fmt.Sprintf("^branch-%d$", i))
		if err != nil {
			t.Fatalf("compile pattern %d: %v", i, err)
		}
	}
	if got := len(regexCache); got != maxRegexCacheEntries {
		t.Fatalf("regex cache size = %d, want %d", got, maxRegexCacheEntries)
	}
}

func TestCompiledRegexCacheUsesHitsAndLRUOrder(t *testing.T) {
	regexCacheMutex.Lock()
	oldCache := regexCache
	oldOrder := regexCacheOrder
	regexCache = make(map[string]*regexp.Regexp, maxRegexCacheEntries)
	regexCacheOrder = nil
	regexCacheMutex.Unlock()
	t.Cleanup(func() {
		regexCacheMutex.Lock()
		regexCache = oldCache
		regexCacheOrder = oldOrder
		regexCacheMutex.Unlock()
	})

	patternA := "^alpha$"
	patternB := "^beta$"
	firstA, err := getCompiledRegex(patternA)
	if err != nil {
		t.Fatalf("compile pattern A: %v", err)
	}
	if _, err := getCompiledRegex(patternB); err != nil {
		t.Fatalf("compile pattern B: %v", err)
	}
	secondA, err := getCompiledRegex(patternA)
	if err != nil {
		t.Fatalf("get cached pattern A again: %v", err)
	}
	if secondA != firstA {
		t.Fatal("cached pattern A did not return the same regexp pointer on hit")
	}

	for i := 0; i < maxRegexCacheEntries-1; i++ {
		_, err := getCompiledRegex(fmt.Sprintf("^branch-%d$", i+10))
		if err != nil {
			t.Fatalf("compile transient pattern %d: %v", i+10, err)
		}
	}
	if _, ok := regexCache[patternB]; ok {
		t.Fatalf("oldest cache entry %q should have been evicted by LRU ordering", patternB)
	}
	if got := regexCache[patternA]; got != secondA {
		t.Fatalf("pattern A should remain cached and reused after eviction; got %p, want %p", got, secondA)
	}
}

func TestFailureClass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result apiv1.ResultEnvelope
		want   string
	}{
		{name: "success", result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, want: OutcomePass},
		{
			name: "retryable infrastructure failure",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "timeout", Retryable: true},
			},
			want: OutcomeInfra,
		},
		{
			name: "business failure",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "command exited 1; stderr: TestWidget failed"},
			},
			want: OutcomeFail,
		},
		{
			name: "golangci lock contention",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "command exited 3; stderr: Parallel golangci-lint is running"},
			},
			want: OutcomeInfra,
		},
		{
			name: "process resource contention",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "fork/exec test binary: resource temporarily unavailable"},
			},
			want: OutcomeInfra,
		},
		{
			name: "dependency download TLS handshake failure",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "npm error OpenSSL/3.6.0: error:0A000410:SSL routines::ssl/tls alert handshake failure"},
			},
			want: OutcomeInfra,
		},
		{
			name: "business TLS handshake failure",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "command exited 1; stderr: TestTLSConfig failed: tls alert handshake failure"},
			},
			want: OutcomeFail,
		},
		{
			name: "persistent TLS certificate failure",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "tls: failed to verify certificate: x509: certificate signed by unknown authority"},
			},
			want: OutcomeFail,
		},
		{
			name: "typed business failure with contention words",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "validation_failed", Message: "resource temporarily unavailable"},
			},
			want: OutcomeFail,
		},

		// #3373: observed 2026-08-20 on the cloud instance. Each of these
		// classified `fail` and routed to implement repasses against a
		// problem no diff can fix.
		{
			name: "observed: playwright install on a read-only browsers mount",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error: &apiv1.ErrorInfo{
					Code:    "nonzero_exit",
					Message: `command exited 1; failure: Error: EROFS: read-only file system, mkdir '/opt/ms-playwright/__dirlock'`,
				},
			},
			want: OutcomeInfra,
		},
		{
			name: "observed: module zip fetch denied by the egress proxy",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error: &apiv1.ErrorInfo{
					Code:    "nonzero_exit",
					Message: `command exited 1; failure: go: golang.org/x/tools/cmd/deadcode@v0.35.0: Get "https://storage.googleapis.com/proxy-golang-org-prod/module/golang.org/x/tools/@v/v0.35.0.zip": Forbidden`,
				},
			},
			want: OutcomeInfra,
		},
		{
			// The third observed message is the make trailer that #3374's
			// last-equal-priority-wins defect substituted for the EROFS
			// diagnostic above. It carries no signature and must not be
			// invented into one — recognizing this failure depends on
			// #3374 delivering the real diagnostic to the classifier.
			name: "observed: stolen make trailer stays unrecognized (needs #3374)",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error: &apiv1.ErrorInfo{
					Code:    "nonzero_exit",
					Message: `command exited 1; failure: ci: portal-playwright-install: exit status 1`,
				},
			},
			want: OutcomeFail,
		},

		// Filesystem errno family.
		{
			name: "go-style read-only filesystem write",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "command exited 1; failure: mkdir /workspace/.cache: read-only file system"},
			},
			want: OutcomeInfra,
		},
		{
			name: "eacces on an install target",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: `command exited 1; failure: Error: EACCES: permission denied, open '/usr/local/lib/node_modules/.package-lock.json'`},
			},
			want: OutcomeInfra,
		},
		{
			name: "go-style permission denied on a directory create",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "command exited 1; failure: mkdir /opt/cache: permission denied"},
			},
			want: OutcomeInfra,
		},
		{
			name: "authorization denial is not a filesystem errno",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "command exited 1; failure: gh: Resource not accessible by integration (permission denied)"},
			},
			want: OutcomeFail,
		},

		// Dependency-transport family: both axes required.
		{
			name: "module proxy returns 403",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: `command exited 1; failure: go: example.com/mod@v1.2.3: reading https://proxy.golang.org/example.com/mod/@v/v1.2.3.zip: 403 Forbidden`},
			},
			want: OutcomeInfra,
		},
		{
			name: "module proxy connection refused",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: `command exited 1; failure: go: downloading example.com/mod v1.2.3: dial tcp 10.0.0.1:443: connect: connection refused`},
			},
			want: OutcomeInfra,
		},
		{
			name: "npm registry i/o timeout",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: `command exited 1; failure: npm error network request to https://registry.npmjs.org/@playwright%2ftest failed, reason: connect ETIMEDOUT 104.16.0.1:443`},
			},
			want: OutcomeInfra,
		},
		{
			// #4141/#4143: the private mirror in a polluted lockfile is on no
			// host list, so the tool's own error prefix is what identifies the
			// fetch.
			name: "npm tarball 403 from a private mirror",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: `command exited 2; failure: npm error 403 403 Forbidden - GET https://ms-feed-12.pkgs.visualstudio.com/1es-public/_packaging/npm-public/npm/registry/three/-/three-0.185.1.tgz`},
			},
			want: OutcomeInfra,
		},
		{
			// The wrapper trailer alone names no cause; it must stay a work
			// failure, which is why internal/executor has to surface the denial
			// line instead of this one.
			name: "make trailer alone is not infrastructure",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: `command exited 2; failure: make: *** [Makefile:352: ci] Error 1`},
			},
			want: OutcomeFail,
		},
		{
			name: "application 403 without a dependency fetch",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: `command exited 1; failure: --- FAIL: TestForbidden: want 200, got 403 Forbidden from https://api.example.com/v1/widgets`},
			},
			want: OutcomeFail,
		},
		{
			name: "dependency fetch host without a transport denial",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: `command exited 1; failure: go: downloading example.com/mod v1.2.3: checksum mismatch against sum.golang.org`},
			},
			want: OutcomeFail,
		},
		{
			name: "typed failure keeps its class despite an infra signature",
			result: apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error:  &apiv1.ErrorInfo{Code: "validation_failed", Message: "read-only file system"},
			},
			want: OutcomeFail,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputs, inputErr := AutomatedInputs(tc.result)
			if inputErr != nil {
				t.Fatalf("AutomatedInputs: %v", inputErr)
			}
			out, err := evalCheck(t, "failure-class", nil, inputs)
			if err != nil || out != tc.want {
				t.Fatalf("got %q, %v; want %q", out, err, tc.want)
			}
			if tc.result.Error != nil {
				if inputs[InputKeyErrorCode] != tc.result.Error.Code ||
					inputs[InputKeyErrorMessage] != tc.result.Error.Message ||
					inputs[InputKeyErrorRetryable] != tc.result.Error.Retryable {
					t.Fatalf("error inputs = %v, want code=%q message=%q retryable=%t", inputs, tc.result.Error.Code, tc.result.Error.Message, tc.result.Error.Retryable)
				}
			}
		})
	}
}

func TestAutomatedInputsRejectsReservedOutputKeys(t *testing.T) {
	subject := apiv1.ResultEnvelope{
		Status: apiv1.ResultFailure,
		Error:  &apiv1.ErrorInfo{Code: "actual", Message: "actual failure", Retryable: true},
		Outputs: map[string]interface{}{
			InputKeyStatus:         "success",
			InputKeyErrorCode:      "forged",
			InputKeyErrorMessage:   "forged success",
			InputKeyErrorRetryable: false,
		},
	}

	inputs, err := AutomatedInputs(subject)
	if err == nil {
		t.Fatal("AutomatedInputs error = nil, want reserved-key collision")
	}
	for _, key := range []string{InputKeyStatus, InputKeyErrorCode, InputKeyErrorMessage, InputKeyErrorRetryable} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("AutomatedInputs error = %q, want reserved key %q", err, key)
		}
	}
	if inputs[InputKeyStatus] != string(apiv1.ResultFailure) ||
		inputs[InputKeyErrorCode] != "actual" ||
		inputs[InputKeyErrorMessage] != "actual failure" ||
		inputs[InputKeyErrorRetryable] != true {
		t.Fatalf("automated inputs = %#v, want runner-owned result fields", inputs)
	}
}

func TestOutputEqualsRequiresParams(t *testing.T) {
	if _, err := evalCheck(t, "output-equals", nil, nil); err == nil {
		t.Fatal("want error for missing params.key/equals")
	}
	if _, err := evalCheck(t, "output-equals", map[string]string{"key": "k"}, nil); err == nil {
		t.Fatal("want error for missing params.equals")
	}
}

func TestOutputEquals(t *testing.T) {
	out, err := evalCheck(t, "output-equals", map[string]string{"key": "branch", "equals": "main"}, map[string]interface{}{"branch": "main"})
	if err != nil || out != OutcomePass {
		t.Fatalf("got %q, %v; want pass", out, err)
	}
	out, err = evalCheck(t, "output-equals", map[string]string{"key": "branch", "equals": "main"}, map[string]interface{}{"branch": "dev"})
	if err != nil || out != OutcomeFail {
		t.Fatalf("got %q, %v; want fail", out, err)
	}
}

func TestOutputNumericGTE(t *testing.T) {
	cases := []struct {
		name      string
		value     interface{}
		threshold string
		want      string
	}{
		{"float above", 85.5, "80", OutcomePass},
		{"float below", 70.0, "80", OutcomeFail},
		{"int equal", 80, "80", OutcomePass},
		{"string numeric", "81", "80", OutcomePass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := evalCheck(t, "output-numeric-gte", map[string]string{"key": "coverage", "threshold": tc.threshold}, map[string]interface{}{"coverage": tc.value})
			if err != nil || out != tc.want {
				t.Fatalf("got %q, %v; want %q", out, err, tc.want)
			}
		})
	}
}

func TestOutputNumericGTEErrorsOnBadInput(t *testing.T) {
	if _, err := evalCheck(t, "output-numeric-gte", map[string]string{"key": "coverage", "threshold": "not-a-number"}, map[string]interface{}{"coverage": 80.0}); err == nil {
		t.Fatal("want error for non-numeric threshold")
	}
	if _, err := evalCheck(t, "output-numeric-gte", map[string]string{"key": "coverage", "threshold": "80"}, map[string]interface{}{"coverage": "nope"}); err == nil {
		t.Fatal("want error for non-numeric input value")
	}
	if _, err := evalCheck(t, "output-numeric-gte", map[string]string{"key": "coverage", "threshold": "80"}, nil); err == nil {
		t.Fatal("want error for missing input value")
	}
}

func TestOutputNumericLTE(t *testing.T) {
	cases := []struct {
		name  string
		value interface{}
		want  string
	}{
		{"float below", 70.0, OutcomePass},
		{"int equal", 80, OutcomePass},
		{"string above", "81", OutcomeFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := evalCheck(t, "output-numeric-lte", map[string]string{"key": "changedFiles", "threshold": "80"}, map[string]interface{}{"changedFiles": tc.value})
			if err != nil || out != tc.want {
				t.Fatalf("got %q, %v; want %q", out, err, tc.want)
			}
		})
	}
}

func TestOutputNumericLTEErrorsOnBadInput(t *testing.T) {
	params := map[string]string{"key": "changedFiles", "threshold": "80"}
	cases := []struct {
		name   string
		params map[string]string
		inputs map[string]interface{}
	}{
		{"missing key param", map[string]string{"threshold": "80"}, nil},
		{"missing threshold param", map[string]string{"key": "changedFiles"}, nil},
		{"non-numeric threshold", map[string]string{"key": "changedFiles", "threshold": "many"}, map[string]interface{}{"changedFiles": 10}},
		{"non-numeric input", params, map[string]interface{}{"changedFiles": "many"}},
		{"unsupported input type", params, map[string]interface{}{"changedFiles": true}},
		{"missing input", params, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := evalCheck(t, "output-numeric-lte", tc.params, tc.inputs); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestOutputNumericLT(t *testing.T) {
	cases := []struct {
		name  string
		value interface{}
		want  string
	}{
		{"float below", 79.5, OutcomePass},
		{"int equal", 80, OutcomeFail},
		{"string above", "81", OutcomeFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := evalCheck(t, "output-numeric-lt", map[string]string{"key": "warnings", "threshold": "80"}, map[string]interface{}{"warnings": tc.value})
			if err != nil || out != tc.want {
				t.Fatalf("got %q, %v; want %q", out, err, tc.want)
			}
		})
	}
}

func TestOutputNumericLTErrorsOnBadInput(t *testing.T) {
	params := map[string]string{"key": "warnings", "threshold": "80"}
	cases := []struct {
		name   string
		params map[string]string
		inputs map[string]interface{}
	}{
		{"missing key param", map[string]string{"threshold": "80"}, nil},
		{"missing threshold param", map[string]string{"key": "warnings"}, nil},
		{"non-numeric threshold", map[string]string{"key": "warnings", "threshold": "many"}, map[string]interface{}{"warnings": 10}},
		{"non-numeric input", params, map[string]interface{}{"warnings": "many"}},
		{"unsupported input type", params, map[string]interface{}{"warnings": true}},
		{"missing input", params, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := evalCheck(t, "output-numeric-lt", tc.params, tc.inputs); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestOutputNotEquals(t *testing.T) {
	cases := []struct {
		name  string
		value interface{}
		want  string
	}{
		{"different string", "dev", OutcomePass},
		{"equal string", "main", OutcomeFail},
		{"flattened integer", 12, OutcomePass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := evalCheck(t, "output-not-equals", map[string]string{"key": "branch", "equals": "main"}, map[string]interface{}{"branch": tc.value})
			if err != nil || out != tc.want {
				t.Fatalf("got %q, %v; want %q", out, err, tc.want)
			}
		})
	}
}

func TestOutputNotEqualsErrorsOnBadInput(t *testing.T) {
	params := map[string]string{"key": "branch", "equals": "main"}
	cases := []struct {
		name   string
		params map[string]string
		inputs map[string]interface{}
	}{
		{"missing key param", map[string]string{"equals": "main"}, nil},
		{"missing equals param", map[string]string{"key": "branch"}, nil},
		{"unsupported input type", params, map[string]interface{}{"branch": []string{"main"}}},
		{"missing input", params, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := evalCheck(t, "output-not-equals", tc.params, tc.inputs); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestOutputMatches(t *testing.T) {
	cases := []struct {
		name    string
		value   interface{}
		pattern string
		want    string
	}{
		{"matching string", "release/v2", `^release/v\d+$`, OutcomePass},
		{"non-matching string", "main", `^release/v\d+$`, OutcomeFail},
		{"flattened boolean", true, `^true$`, OutcomePass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := evalCheck(t, "output-matches", map[string]string{"key": "branch", "pattern": tc.pattern}, map[string]interface{}{"branch": tc.value})
			if err != nil || out != tc.want {
				t.Fatalf("got %q, %v; want %q", out, err, tc.want)
			}
		})
	}
}

func TestOutputMatchesErrorsOnBadInput(t *testing.T) {
	params := map[string]string{"key": "branch", "pattern": `^release/`}
	cases := []struct {
		name   string
		params map[string]string
		inputs map[string]interface{}
	}{
		{"missing key param", map[string]string{"pattern": `.*`}, nil},
		{"missing pattern param", map[string]string{"key": "branch"}, nil},
		{"invalid pattern", map[string]string{"key": "branch", "pattern": `(`}, map[string]interface{}{"branch": "main"}},
		{"unsupported input type", params, map[string]interface{}{"branch": map[string]string{"name": "main"}}},
		{"missing input", params, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := evalCheck(t, "output-matches", tc.params, tc.inputs); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestCIStatusCheck(t *testing.T) {
	// Default vocabulary is providers.CheckState ("passing"/"failing"/
	// "pending" — internal/gate does not import providers, see
	// checkEqualsVocab's doc comment), not apiv1.ResultStatus's "success"
	// (#132: a ci-poll stage emits "passing"/"failing", so the check must
	// default to matching that, not the unrelated ResultStatus vocabulary).
	out, err := evalCheck(t, "ci-status", nil, map[string]interface{}{"ciStatus": "passing"})
	if err != nil || out != OutcomePass {
		t.Fatalf("got %q, %v; want pass", out, err)
	}
	out, err = evalCheck(t, "ci-status", nil, map[string]interface{}{"ciStatus": "pending"})
	if err != nil || out != OutcomeFail {
		t.Fatalf("got %q, %v; want fail", out, err)
	}
}

// TestCIStatusCheckTimeoutIsADistinctOutcome is the routing regression test
// for #239: a ci-poll timeout (executor.CIStatusTimeout) must resolve to its
// own OutcomeTimeout, not get folded into OutcomeFail — a workflow's ci-gate
// needs to tell "CI ran and failed" apart from "the poll never reached a
// terminal state" so it can route the latter to escalation instead of an
// implement repass.
func TestCIStatusCheckTimeoutIsADistinctOutcome(t *testing.T) {
	out, err := evalCheck(t, "ci-status", nil, map[string]interface{}{"ciStatus": executor.CIStatusTimeout})
	if err != nil || out != OutcomeTimeout {
		t.Fatalf("got %q, %v; want %q", out, err, OutcomeTimeout)
	}
	if out == OutcomeFail {
		t.Fatal("a poll timeout must not resolve to the same outcome as a genuine CI failure")
	}
}

// TestCIGateRoutesTimeoutOutcomeToEscalate proves the full gate.Evaluate path
// (not just the check function) resolves a ci-poll timeout to @escalate when
// a workflow's ci-gate declares a "timeout" branch — the routing half of
// #239, exercised through the same Evaluator a real run uses.
func TestCIGateRoutesTimeoutOutcomeToEscalate(t *testing.T) {
	g := apiv1.Gate{
		Name:      "ci-gate",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "ci-status"},
		Branches: map[string]string{
			OutcomePass:    "close-out",
			OutcomeFail:    "implement",
			OutcomeTimeout: wf.TargetEscalate,
		},
	}
	e := &Evaluator{Automated: NewAutomatedEvaluator()}
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{"ciStatus": executor.CIStatusTimeout}}

	result, err := e.Evaluate(context.Background(), g, env, "ci-poll", apiv1.ResultEnvelope{Status: apiv1.ResultFailure}, "", false)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Outcome != OutcomeTimeout {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeTimeout)
	}
	if result.Target != wf.TargetEscalate {
		t.Fatalf("target = %q, want %q (not the implement repass)", result.Target, wf.TargetEscalate)
	}
}

// TestLandOutcomeCheck pins issue #758's merge-policy writeback
// distinction: merge-pr's Outputs["landOutcome"] of "merged"/"enqueued"
// resolves to that same outcome, and anything else (including the unmet-
// conjunct refusal case, which sets no landOutcome key at all) resolves to
// "fail" — never a silent default to one of the two success outcomes.
func TestLandOutcomeCheck(t *testing.T) {
	out, err := evalCheck(t, "land-outcome", nil, map[string]interface{}{"landOutcome": "merged"})
	if err != nil || out != OutcomeMerged {
		t.Fatalf("got %q, %v; want %q", out, err, OutcomeMerged)
	}
	out, err = evalCheck(t, "land-outcome", nil, map[string]interface{}{"landOutcome": "enqueued"})
	if err != nil || out != OutcomeEnqueued {
		t.Fatalf("got %q, %v; want %q", out, err, OutcomeEnqueued)
	}
	out, err = evalCheck(t, "land-outcome", nil, nil)
	if err != nil || out != OutcomeFail {
		t.Fatalf("got %q, %v; want %q for a missing landOutcome (the refusal case)", out, err, OutcomeFail)
	}
}

// TestQueueOutcomeCheck pins issue #758's three-way merge-queue-poll
// writeback: "merged"/"evicted"/"timeout" each resolve to themselves —
// eviction distinct from both a genuine merge and a still-pending timeout —
// and anything else resolves to "fail".
func TestQueueOutcomeCheck(t *testing.T) {
	cases := []struct {
		queueOutcome string
		want         string
	}{
		{"merged", OutcomeMerged},
		{"evicted", OutcomeEvicted},
		{"timeout", OutcomeTimeout},
		{"garbage", OutcomeFail},
		{"", OutcomeFail},
	}
	for _, tc := range cases {
		out, err := evalCheck(t, "queue-outcome", nil, map[string]interface{}{"queueOutcome": tc.queueOutcome})
		if err != nil || out != tc.want {
			t.Fatalf("queueOutcome=%q: got %q, %v; want %q", tc.queueOutcome, out, err, tc.want)
		}
	}
}

// TestQueueGateRoutesEvictedOutcomeToRemediation is the routing regression
// test for #758's eviction acceptance criterion: an evicted merge-queue
// entry must resolve to its own distinct outcome and target, not fold into
// "fail" the same way a genuine build failure would, and not "timeout" the
// same way a still-pending entry would.
func TestQueueGateRoutesEvictedOutcomeToRemediation(t *testing.T) {
	g := apiv1.Gate{
		Name:      "queue-gate",
		Evaluator: apiv1.EvaluatorAutomated,
		Automated: &apiv1.AutomatedGate{Check: "queue-outcome"},
		Branches: map[string]string{
			OutcomeMerged:  "post-merge",
			OutcomeEvicted: "mark-queue-evicted",
			OutcomeTimeout: "",
			OutcomeFail:    "",
		},
	}
	e := &Evaluator{Automated: NewAutomatedEvaluator()}
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{"queueOutcome": "evicted"}}

	result, err := e.Evaluate(context.Background(), g, env, "merge-queue-poll", apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, "", false)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if result.Outcome != OutcomeEvicted {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeEvicted)
	}
	if result.Target != "mark-queue-evicted" {
		t.Fatalf("target = %q, want %q (not silently merged into the post-merge or dead-end branches)", result.Target, "mark-queue-evicted")
	}
}

func TestUnknownCheckErrors(t *testing.T) {
	if _, err := evalCheck(t, "no-such-check", nil, nil); err == nil {
		t.Fatal("want error for unknown check name")
	}
}

func TestEvaluateVerdictMapsOutcomeToDecision(t *testing.T) {
	e := NewAutomatedEvaluator()
	env := apiv1.InvocationEnvelope{Inputs: map[string]interface{}{InputKeyStatus: "success"}}
	v, err := e.EvaluateVerdict(context.Background(), apiv1.AutomatedGate{Check: "status-equals"}, env)
	if err != nil || v.Decision != apiv1.VerdictPass {
		t.Fatalf("got %+v, %v; want VerdictPass", v, err)
	}

	env.Inputs[InputKeyStatus] = "failure"
	v, err = e.EvaluateVerdict(context.Background(), apiv1.AutomatedGate{Check: "status-equals"}, env)
	if err != nil || v.Decision != apiv1.VerdictFail {
		t.Fatalf("got %+v, %v; want VerdictFail", v, err)
	}
}

func TestEvaluateVerdictPropagatesError(t *testing.T) {
	e := NewAutomatedEvaluator()
	if _, err := e.EvaluateVerdict(context.Background(), apiv1.AutomatedGate{Check: "no-such-check"}, apiv1.InvocationEnvelope{}); err == nil {
		t.Fatal("want error propagated from Evaluate")
	}
}
