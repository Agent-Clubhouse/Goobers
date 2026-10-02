package runtimeplan

// Cleanup records narrowly scoped evidence. A passed local fixture is never
// authority to start another writer or evidence of remote work termination.
type Cleanup struct {
	Ownership                  string `json:"ownership"`
	Owner                      string `json:"owner"`
	Code                       string `json:"code"`
	Outcome                    string `json:"outcome"`
	RequiredGuaranteeSatisfied bool   `json:"requiredGuaranteeSatisfied"`
	AuthorizesWriter           bool   `json:"authorizesWriter"`
	Detail                     string `json:"detail"`
}

// CleanupObservation accepts an observed stop only for work owned by the
// observer. Host-owned, unsupported and unknown cleanup always fail closed.
func CleanupObservation(ownership, owner string, stopped bool) Cleanup {
	result := Cleanup{Ownership: ownership, Owner: owner, Code: "remote_cleanup_unobservable", Outcome: "unobservable", Detail: "cleanup was not observed; cannot satisfy a required cleanup guarantee"}
	switch ownership {
	case "owned":
		result.Code = "owned_cleanup_unobservable"
		if stopped {
			result.Code, result.Outcome = "owned_cleanup_observed", "passed"
			result.RequiredGuaranteeSatisfied = true
			result.Detail = "forced stop observed for the owned fixture descendants only; target daemon/worker and remote work remain unobservable"
		}
	case "host-owned":
		result.Code = "cleanup_host_owned"
		result.Detail = "cleanup belongs to the execution host; reporting-process evidence cannot prove host cleanup"
	case "unsupported":
		result.Code, result.Outcome = "cleanup_guarantee_unavailable", "unsupported"
		result.Detail = "no supported cleanup guarantee is available for this scope"
	default:
		result.Ownership = "unobservable"
	}
	return result
}
