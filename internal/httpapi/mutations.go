package httpapi

import (
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
)

// registerMutationRoutes wires the tier-2 human-intervention actions
// (approve/override/rerun — HITL-7/#469) through the exact same
// Authenticate-then-Authorize seam every read route already passes through:
// Router.Handle applies both unconditionally, before any handler runs, so
// registering a route here is sufficient to gate it — no separate mutation
// auth path exists or is needed.
//
// Each handler body is a deliberate stub. The real primitives this seam will
// eventually front are split across other issues: rerun-with-addendum and
// resume-from-terminal already exist internally (#465/#467,
// internal/runner.RerunStage and friends) but nothing outside internal/runner
// calls them yet, and gate force-pass/override (#468) is entirely unbuilt —
// internal/gate/evaluate.go still refuses apiv1.EvaluatorHuman outright.
// Landing the route surface now, ungated by those, means #466 (CLI
// intervention surface) and #468 only ever fill in a handler body — neither
// ever touches auth wiring.
func registerMutationRoutes(router *Router) {
	router.Handle(apicontract.RouteApproveStage, stageMutationStub("approve"))
	router.Handle(apicontract.RouteOverrideStage, stageMutationStub("override"))
	router.Handle(apicontract.RouteRerunStage, stageMutationStub("rerun"))
}

// stageMutationStub proves a tier-2 action is reachable only through the
// access-control seam while its real behavior is still unimplemented
// (#466/#468). A request that passes Authenticate+Authorize reaches here and
// gets a clear, typed 501 — never a silent 404 (as if the route did not
// exist) and never a fake 200 (implying a mutation happened when none did).
func stageMutationStub(capability string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			capability+" is not implemented yet (tracked separately from the access-control seam, HITL-7/#469)")
	}
}
