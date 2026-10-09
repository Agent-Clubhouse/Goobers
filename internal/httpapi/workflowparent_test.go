package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkflowParentCannotInheritOrdinaryAuthority(t *testing.T) {
	for _, route := range podRouteTable() {
		t.Run(route.name, func(t *testing.T) {
			principal := Principal{Subject: podPrincipalSubject("parent"), Issuer: WorkflowParentPrincipalIssuer, WorkflowParent: &WorkflowParentPrincipal{ContractDigest: "sha256:" + strings.Repeat("a", 64)}, Roles: []Role{RoleAdmin, RoleOperate, RoleView}, Scopes: knownPodScopes()}
			request := httptest.NewRequest(route.method, route.path, nil)
			request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
			wantBlob := authorizeWorkerBlob(request) == nil
			if IsPodPrincipal(principal) || (RequireRoles().Authorize(request) == nil) != wantBlob {
				t.Fatal("uninstalled parent owner inherited ordinary authority")
			}
		})
	}
}
