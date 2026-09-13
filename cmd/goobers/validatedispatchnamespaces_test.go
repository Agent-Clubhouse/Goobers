package main

import (
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"

	"github.com/goobers/goobers/internal/dispatcher"
)

// #4897 acceptance criterion 3: `goobers validate` warns when a gaggle
// declares a per-gaggle namespace the current dispatch configuration cannot
// honor. This is advisory-only and never changes the exit code — the same
// posture --check-repos takes, since a config that is otherwise valid must
// stay valid with no cluster in reach.

func TestValidateCheckDispatchNamespacesReadyEmitsNoWarning(t *testing.T) {
	root := initDemoForValidate(t)

	previousClient := dispatchKubeClient
	dispatchKubeClient = func() (kubernetes.Interface, error) { return nil, nil }
	t.Cleanup(func() { dispatchKubeClient = previousClient })
	previousPreflight := preflightGaggleNamespaces
	preflightGaggleNamespaces = func(context.Context, kubernetes.Interface, map[string]string) ([]dispatcher.NamespacePreflightResult, error) {
		return []dispatcher.NamespacePreflightResult{{Namespace: "gaggle-example", Exists: true}}, nil
	}
	t.Cleanup(func() { preflightGaggleNamespaces = previousPreflight })

	code, stdout, stderr := runArgs(t, "validate", "--check-dispatch-namespaces", root)
	if code != 0 {
		t.Fatalf("code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "GNS001") {
		t.Fatalf("unexpected GNS001 warning for a ready namespace:\n%s", stdout)
	}
}

func TestValidateCheckDispatchNamespacesWarnsWithoutFailingExitCode(t *testing.T) {
	root := initDemoForValidate(t)

	previousClient := dispatchKubeClient
	dispatchKubeClient = func() (kubernetes.Interface, error) { return nil, nil }
	t.Cleanup(func() { dispatchKubeClient = previousClient })
	previousPreflight := preflightGaggleNamespaces
	preflightGaggleNamespaces = func(context.Context, kubernetes.Interface, map[string]string) ([]dispatcher.NamespacePreflightResult, error) {
		result := []dispatcher.NamespacePreflightResult{{Namespace: "gaggle-example", Exists: false}}
		return result, errNamespacePreflight
	}
	t.Cleanup(func() { preflightGaggleNamespaces = previousPreflight })

	code, stdout, stderr := runArgs(t, "validate", "--check-dispatch-namespaces", root)
	if code != 0 {
		t.Fatalf("a live cluster-reachability finding must stay advisory: code=%d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "GNS001") || !strings.Contains(stdout, "gaggle-example") {
		t.Fatalf("stdout missing the GNS001 warning naming the unready namespace:\n%s", stdout)
	}
}

func TestValidateCheckDispatchNamespacesSkipsSilentlyWithNoClusterCredentials(t *testing.T) {
	root := initDemoForValidate(t)

	previousClient := dispatchKubeClient
	dispatchKubeClient = func() (kubernetes.Interface, error) { return nil, errNoClusterCredentials }
	t.Cleanup(func() { dispatchKubeClient = previousClient })

	code, stdout, stderr := runArgs(t, "validate", "--check-dispatch-namespaces", root)
	if code != 0 {
		t.Fatalf("code=%d, want 0 (no cluster credentials must not fail an otherwise-valid config); stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "GNS001") {
		t.Fatalf("no cluster credentials must never produce a GNS001 finding:\n%s", stdout)
	}
}

// initDemoForValidate scaffolds a demo instance whose "example" gaggle
// declares isolation.namespace: gaggle-example (the starter scaffold's
// default), which is what the tests above assert against.
func initDemoForValidate(t *testing.T) string {
	t.Helper()
	return initDemo(t)
}

type sentinelError string

func (e sentinelError) Error() string { return string(e) }

const (
	errNamespacePreflight   = sentinelError("dispatcher: namespace \"gaggle-example\": missing RBAC grants")
	errNoClusterCredentials = sentinelError("load kubernetes config for stage dispatch: no configuration has been provided")
)
