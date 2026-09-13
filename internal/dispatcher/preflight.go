package dispatcher

import (
	"context"
	"errors"
	"fmt"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// namespaceGrant is one verb this dispatcher's own credentials must hold in
// every namespace it dispatches into. This list is deliberately the exact
// verb set kubePodAPI calls (kubepods.go) — the same narrow Role
// deploy/reference/gaggle-namespace/base/dispatcher-rbac.yaml renders — so a
// grant this package never uses is never demanded, and one it depends on is
// never missed.
var namespaceGrants = []struct{ group, resource, verb string }{
	{"", "pods", "create"},
	{"", "pods", "get"},
	{"", "pods", "delete"},
	{"", "pods", "list"},
	{"apps", "deployments", "get"},
}

// NamespacePreflightResult is one gaggle namespace's dispatch readiness.
type NamespacePreflightResult struct {
	// Namespace is the k8s namespace checked.
	Namespace string
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
// right now: it exists and every required grant is held.
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
func PreflightNamespaces(ctx context.Context, client kubernetes.Interface, gaggleNamespaces map[string]string) ([]NamespacePreflightResult, error) {
	namespaces := distinctNamespaces(gaggleNamespaces)
	results := make([]NamespacePreflightResult, 0, len(namespaces))
	var errs []error
	for _, ns := range namespaces {
		result := preflightOneNamespace(ctx, client, ns)
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
