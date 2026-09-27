package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/credreadiness"
	"github.com/goobers/goobers/internal/harness"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

// checkHarnessWithCredential runs --check-harness's harness sweep against a
// fake adapter and one supplied agent:model grant, returning what an operator
// would see.
func checkHarnessWithCredential(t *testing.T, credential harnessModelCredential) (string, bool) {
	t.Helper()
	withHarnessAdapter(t, func(h apiv1.Harness, _ harness.EnvironmentConfig, _ map[string][]string, _ func(context.Context) (string, error)) (harness.Adapter, error) {
		return &harnesstest.FakeAdapter{AdapterName: string(h)}, nil
	})
	goobers := []apiv1.Goober{{Spec: apiv1.GooberSpec{Harness: apiv1.HarnessCopilot}}}
	var out, errOut strings.Builder
	ok := checkHarnessesAtSources(goobers, &out, &errOut, nil, harness.EnvironmentConfig{}, nil,
		func(apiv1.Harness) (harnessModelCredential, error) { return credential, nil })
	return out.String(), ok
}

// #5261: a harness that signs in successfully says nothing about WHICH
// credential source backed it. The readiness line reports the source kind and
// name -- never the value -- alongside the existing OK.
func TestCheckHarnessReportsCredentialSourceKind(t *testing.T) {
	const secret = "ghp_thisisthetokenvalue"
	stdout, ok := checkHarnessWithCredential(t, harnessModelCredential{
		Resolve:  func(context.Context) (string, error) { return secret, nil },
		Ref:      credentials.TokenRef{Name: "agent:model", Env: "GOOBERS_MODEL_PAT"},
		RefFound: true,
		Label:    "credentials[] agent:model (unscoped)",
	})
	if !ok {
		t.Fatalf("check failed: %s", stdout)
	}
	for _, want := range []string{"HARNESS copilot: OK", "credential agent:model", string(credreadiness.StatusUsable), "env GOOBERS_MODEL_PAT"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, secret) {
		t.Fatalf("the credential value reached operator output:\n%s", stdout)
	}
}

// A user-scoped source that resolves for the operator must say so rather than
// reading as a blanket pass: the service identity a scheduled run uses may not
// be able to read the same keychain item at all.
func TestCheckHarnessFlagsUserScopedCredentialSource(t *testing.T) {
	stdout, ok := checkHarnessWithCredential(t, harnessModelCredential{
		Resolve:  func(context.Context) (string, error) { return "tok", nil },
		Ref:      credentials.TokenRef{Name: "agent:model", Keychain: "goobers-model"},
		RefFound: true,
	})
	if !ok {
		t.Fatalf("check failed: %s", stdout)
	}
	if !strings.Contains(stdout, string(credreadiness.SourceKeychain)) {
		t.Fatalf("source kind not reported:\n%s", stdout)
	}
	if !strings.Contains(stdout, "service identity may not share this source") {
		t.Fatalf("a user-scoped source read as an unqualified pass:\n%s", stdout)
	}
}

// A credential that cannot be refreshed is reported with its own stable code,
// and does NOT fail the harness check -- sign-in and credential refresh are
// different observations, which is the distinction #5261 is about.
func TestCheckHarnessReportsRefreshFailureDistinctly(t *testing.T) {
	stdout, ok := checkHarnessWithCredential(t, harnessModelCredential{
		Resolve: func(context.Context) (string, error) {
			return "", fmt.Errorf("interactive sign-in required: %w", credreadiness.ErrRefreshFailed)
		},
		Ref:      credentials.TokenRef{Name: "agent:model", File: "/etc/goobers/model-pat"},
		RefFound: true,
	})
	if !ok {
		t.Fatalf("a credential-refresh failure failed the harness sign-in check:\n%s", stdout)
	}
	if !strings.Contains(stdout, string(credreadiness.StatusRefreshFailed)) {
		t.Fatalf("refresh failure not reported with its stable code:\n%s", stdout)
	}
}

// "Nothing was checked" must not render as "checked and fine".
func TestCheckHarnessReportsMissingGrantAsUnobservable(t *testing.T) {
	stdout, ok := checkHarnessWithCredential(t, harnessModelCredential{})
	if !ok {
		t.Fatalf("check failed: %s", stdout)
	}
	if !strings.Contains(stdout, string(credreadiness.StatusUnobservable)) {
		t.Fatalf("an unconfigured grant did not report as unobservable:\n%s", stdout)
	}
}

// The readiness line names the identity it was observed under, so a report
// copied out of one operator's terminal cannot be read as a claim about the
// service account.
func TestCheckHarnessReadinessNamesObservingIdentity(t *testing.T) {
	stdout, _ := checkHarnessWithCredential(t, harnessModelCredential{
		Resolve:  func(context.Context) (string, error) { return "tok", nil },
		Ref:      credentials.TokenRef{Name: "agent:model", Env: "GOOBERS_MODEL_PAT"},
		RefFound: true,
	})
	observer := credreadiness.CurrentObserver()
	if !strings.Contains(stdout, "observed as "+observer.String()) {
		t.Fatalf("readiness line does not name the observing identity (%s):\n%s", observer, stdout)
	}
}

// The discriminating case for the sentinel: a source that cannot be OBSERVED
// under this identity is not a source that failed. Only the wrapped sentinel
// separates them -- an unrecognized error falls to refresh_failed -- so this is
// the test that proves the classification is doing real work.
func TestCheckHarnessSeparatesUnobservableFromFailed(t *testing.T) {
	unobservable, _ := checkHarnessWithCredential(t, harnessModelCredential{
		Resolve: func(context.Context) (string, error) {
			return "", fmt.Errorf("LocalSystem cannot read the login keychain: %w", credreadiness.ErrUnobservable)
		},
		Ref:      credentials.TokenRef{Name: "agent:model", Keychain: "goobers-model"},
		RefFound: true,
	})
	if !strings.Contains(unobservable, string(credreadiness.StatusUnobservable)) {
		t.Fatalf("an unobservable source was not reported as such:\n%s", unobservable)
	}
	if strings.Contains(unobservable, string(credreadiness.StatusRefreshFailed)) {
		t.Fatalf("an unobservable source was reported as a failure:\n%s", unobservable)
	}

	// Same shape, no sentinel: this one IS a failure.
	failed, _ := checkHarnessWithCredential(t, harnessModelCredential{
		Resolve: func(context.Context) (string, error) {
			return "", errors.New("keychain item not found")
		},
		Ref:      credentials.TokenRef{Name: "agent:model", Keychain: "goobers-model"},
		RefFound: true,
	})
	if !strings.Contains(failed, string(credreadiness.StatusRefreshFailed)) {
		t.Fatalf("an unrecognized resolver error did not fall to refresh_failed:\n%s", failed)
	}
}

// The report summary must never upgrade an unobservable source to a pass, and
// must scope its claim to the identity that made it.
func TestCheckHarnessCredentialSummaryDoesNotRoundUp(t *testing.T) {
	unobservable, _ := checkHarnessWithCredential(t, harnessModelCredential{
		Resolve: func(context.Context) (string, error) {
			return "", fmt.Errorf("not readable here: %w", credreadiness.ErrUnobservable)
		},
		Ref:      credentials.TokenRef{Name: "agent:model", Keychain: "goobers-model"},
		RefFound: true,
	})
	if !strings.Contains(unobservable, "not usable as") {
		t.Fatalf("an unobservable source did not keep the report unready:\n%s", unobservable)
	}

	usable, _ := checkHarnessWithCredential(t, harnessModelCredential{
		Resolve:  func(context.Context) (string, error) { return "tok", nil },
		Ref:      credentials.TokenRef{Name: "agent:model", Env: "GOOBERS_MODEL_PAT"},
		RefFound: true,
	})
	if !strings.Contains(usable, "not evidence for another identity") {
		t.Fatalf("a passing report did not scope its claim to the observing identity:\n%s", usable)
	}
}
