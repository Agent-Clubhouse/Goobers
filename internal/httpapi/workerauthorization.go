package httpapi

import (
	"errors"
	"net/http"
	"strings"
)

// Worker transport credentials have disjoint grants regardless of roles.
func authorizeWorkerBlob(request *http.Request) error {
	if (request.Method == http.MethodGet || request.Method == http.MethodPut) && blobPlanePath(request.URL.Path) {
		return nil
	}
	return errors.New("worker blob principal may only get or put blobs")
}

func authorizeWorkerSurrender(request *http.Request) error {
	if request.Method == http.MethodGet && (surrenderPlanePath(request.URL.Path) || strings.HasSuffix(request.URL.Path, "/seen") && surrenderPlanePath(strings.TrimSuffix(request.URL.Path, "/seen"))) {
		return nil
	}
	return errors.New("worker surrender principal may only read surrender results and presence")
}
