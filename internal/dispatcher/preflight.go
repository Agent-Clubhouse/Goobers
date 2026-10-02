package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// namespaceGrant is one verb this dispatcher's own credentials must hold in
// every namespace it dispatches into. This list is deliberately the exact
// verb set kubePodAPI and account preflight call — the same narrow Role
// deploy/reference/gaggle-namespace/base/dispatcher-rbac.yaml renders — so a
// grant this package never uses is never demanded, and one it depends on is
// never missed. The one exception is persistentvolumeclaims get (#5595): the
// Role grants it, but it is not demanded here because a worker without it
// still dispatches, with an emptyDir Go module cache instead of the claim.
// Demanding it would stop an upgraded worker whose namespaces predate it.
var namespaceGrants = []struct{ group, resource, verb string }{
	{"", "pods", "create"},
	{"", "pods", "get"},
	{"", "pods", "delete"},
	{"", "pods", "list"},
	{"apps", "deployments", "get"},
	{"", "serviceaccounts", "get"},
}

// NamespacePreflightResult is one gaggle namespace's dispatch readiness.
type NamespacePreflightResult struct {
	// Namespace is the k8s namespace checked.
	Namespace string
	// ServerVersion records the discovered API server version.
	ServerVersion string
	// ServiceAccounts lists the effective accounts checked in this namespace.
	ServiceAccounts []string
	// Exists is true once the namespace itself was read successfully.
	Exists bool
	// MissingGrants lists "<group>/<resource>:<verb>" for every namespaceGrants
	// entry a SelfSubjectAccessReview reported NOT allowed.
	MissingGrants []string
	// Err is the first error encountered checking this namespace (existence
	// or a review call itself failing), nil when both readiness checks ran to
	// completion (regardless of whether grants were found missing).
	Err error
}

// Ready reports whether stage pods can be dispatched into this namespace
// right now: it exists, the server supports spec.os, every selected account
// disables automount, and every required grant is held.
func (r NamespacePreflightResult) Ready() bool {
	return r.Err == nil && r.Exists && len(r.MissingGrants) == 0
}

// PreflightNamespaces checks, for every DISTINCT namespace a worker is about
// to dispatch stage pods into, that the namespace exists and that this
// process's own credentials hold every verb the dispatcher's create/cleanup
// path calls in it (#4897's startup contract: "validate that every declared
// namespace exists and that the required service account/RBAC permissions
// are available. Fail startup clearly rather than discovering missing access
// after accepting a stage."). A stage already claimed from a backlog item and
// then refused mid-dispatch is a worse failure than refusing to start at all.
//
// Namespaces are checked in sorted order so results are deterministic; the
// returned error joins every namespace's failure, naming each one.
func PreflightNamespaces(ctx context.Context, client kubernetes.Interface, gaggleNamespaces map[string]string, gaggleAccounts ...map[string]string) ([]NamespacePreflightResult, error) {
	namespaces := distinctNamespaces(gaggleNamespaces)
	results := make([]NamespacePreflightResult, 0, len(namespaces))
	var errs []error
	version, versionErr := supportedPodOSVersion(client)
	accounts := map[string]string{}
	if len(gaggleAccounts) > 0 {
		accounts = gaggleAccounts[0]
	}
	for _, ns := range namespaces {
		result := preflightOneNamespace(ctx, client, ns)
		result.ServerVersion = version
		for gaggle, namespace := range gaggleNamespaces {
			if namespace == ns {
				account := (Config{GaggleServiceAccounts: accounts}).serviceAccountFor(gaggle)
				if !slices.Contains(result.ServiceAccounts, account) {
					result.ServiceAccounts = append(result.ServiceAccounts, account)
				}
			}
		}
		slices.Sort(result.ServiceAccounts)
		if result.Err == nil {
			result.Err = errors.Join(versionErr, preflightServiceAccounts(ctx, client, ns, result.ServiceAccounts))
		}
		if result.Err != nil {
			errs = append(errs, result.Err)
		} else if len(result.MissingGrants) > 0 {
			errs = append(errs, fmt.Errorf("dispatcher: namespace %q is missing required RBAC grant(s) %v for this worker's credentials", ns, result.MissingGrants))
		}
		results = append(results, result)
	}
	return results, errors.Join(errs...)
}

func preflightOneNamespace(ctx context.Context, client kubernetes.Interface, namespace string) NamespacePreflightResult {
	result := NamespacePreflightResult{Namespace: namespace}
	if _, err := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{}); err != nil {
		result.Err = fmt.Errorf("dispatcher: namespace %q: %w", namespace, err)
		return result
	}
	result.Exists = true
	for _, grant := range namespaceGrants {
		review := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: namespace,
					Group:     grant.group,
					Resource:  grant.resource,
					Verb:      grant.verb,
				},
			},
		}
		reviewed, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			result.Err = fmt.Errorf("dispatcher: namespace %q: check %s/%s:%s: %w", namespace, grant.group, grant.resource, grant.verb, err)
			return result
		}
		if !reviewed.Status.Allowed {
			result.MissingGrants = append(result.MissingGrants, fmt.Sprintf("%s/%s:%s", grant.group, grant.resource, grant.verb))
		}
	}
	return result
}

// MinPodOSMinor is the Kubernetes 1.x floor for unconditional GA spec.os rendering.
const MinPodOSMinor = 25

func supportedPodOSVersion(client kubernetes.Interface) (string, error) {
	info, err := client.Discovery().ServerVersion()
	if err != nil {
		return "", fmt.Errorf("dispatcher: K8S_POD_OS_VERSION: cannot discover server version: %w", err)
	}
	major, majorErr := strconv.Atoi(info.Major)
	minor, minorErr := strconv.Atoi(strings.TrimSuffix(info.Minor, "+"))
	if majorErr != nil || minorErr != nil || major < 1 || (major == 1 && minor < MinPodOSMinor) {
		return info.GitVersion, fmt.Errorf("dispatcher: K8S_POD_OS_VERSION: server %s (%s.%s) requires Kubernetes >=1.%d for stage pod spec.os", info.GitVersion, info.Major, info.Minor, MinPodOSMinor)
	}
	return info.GitVersion, nil
}

func preflightServiceAccounts(ctx context.Context, client kubernetes.Interface, namespace string, accounts []string) error {
	var errs []error
	for _, account := range accounts {
		sa, err := client.CoreV1().ServiceAccounts(namespace).Get(ctx, account, metav1.GetOptions{})
		if err != nil {
			errs = append(errs, fmt.Errorf("dispatcher: STAGE_SERVICE_ACCOUNT: namespace %q account %q: %w; apply deploy/reference/gaggle-namespace/base/serviceaccount.yaml in this namespace (or provision isolation.serviceAccount)", namespace, account, err))
		} else if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
			errs = append(errs, fmt.Errorf("dispatcher: STAGE_SERVICE_ACCOUNT: namespace %q account %q must set automountServiceAccountToken: false", namespace, account))
		}
	}
	return errors.Join(errs...)
}
