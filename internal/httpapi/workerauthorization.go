package httpapi

import (
	"errors"
	"net/http"
	"strings"
)

// workerPlaneAuthorizers confines single-plane worker credentials. Each
// issuer's grant is disjoint and ignores roles and scopes.
var workerPlaneAuthorizers = map[string]func(*http.Request) error{
	WorkerSurrenderPrincipalIssuer:      authorizeWorkerSurrender,
	WorkerChildAdmissionPrincipalIssuer: authorizeWorkerChildAdmission,
}

// Worker transport credentials have disjoint grants regardless of roles.
func authorizeWorkerBlob(request *http.Request) error {
	if (request.Method == http.MethodGet || request.Method == http.MethodPut) && blobPlanePath(request.URL.Path) {
		return nil
	}
	return errors.New("worker blob principal may only get or put blobs")
}

func authorizeWorkerChildAdmission(request *http.Request) error {
	if request.Method == http.MethodPost && engineChildPlanePath(request.URL.Path) {
		return nil
	}
	return errors.New("worker child-admission principal may only call the engine child-admission plane")
}

func authorizeWorkerSurrender(request *http.Request) error {
	if request.Method == http.MethodGet && (surrenderPlanePath(request.URL.Path) || strings.HasSuffix(request.URL.Path, "/seen") && surrenderPlanePath(strings.TrimSuffix(request.URL.Path, "/seen"))) {
		return nil
	}
	return errors.New("worker surrender principal may only read surrender results and presence")
}
