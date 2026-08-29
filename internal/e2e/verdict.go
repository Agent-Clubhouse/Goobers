package e2e

// Verdict is the smoke's three-way outcome (goobernetes-smoke.md §5 rule 2,
// decision record D4): pass, fail, or INVALID. Invalid is not a third
// severity of fail — it means the criterion's own observer machinery never
// established what it needed to (an SSE capture lost, a journal unreadable,
// a run that never reached the precondition it claims to exercise), and per
// D4 that case "is never counted as passed, never as failed." This is the
// same discipline docs/design/e2e-soak-harness.md §8 already argues:
// apiv1.ResultNoWork's doc comment ("this status is only for 'correctly
// found nothing,' ... never a masked error") and
// internal/readservice.StagePopulation's nil-vs-empty distinction. The smoke
// doc explicitly adopts that vocabulary rather than inventing a second one
// (D4: "a second vocabulary for the same contract is drift").
type Verdict string

const (
	// VerdictPass means the item's observer was consulted and the criterion
	// held.
	VerdictPass Verdict = "pass"
	// VerdictFail means the item's observer was consulted and the criterion
	// did NOT hold — a genuine, successfully-observed negative result. Never
	// silently reclassified as invalid: a real fail must stay a fail
	// (mirrors StagePopulation's "a genuine zero... never silently
	// reclassified as 'no data'").
	VerdictFail Verdict = "fail"
	// VerdictInvalid means the item's observer machinery itself failed
	// before it could report pass or fail. Blocks the exit exactly like a
	// fail (goobernetes-smoke.md §5 rule 2: "blocks the exit"), but is
	// reported and remediated differently — the harness broke, not
	// (necessarily) the product.
	VerdictInvalid Verdict = "invalid"
)

// PreconditionFailure names why a smoke item's observer machinery could not
// establish evidence — e.g. "SSE capture lost", "journal unreadable for run
// <id>", "S9 probe pod never started". A non-empty value always yields
// VerdictInvalid from ClassifyItem, regardless of what else is true.
type PreconditionFailure string

// ClassifyItem is the ONE decision point that turns a raw observation into a
// Verdict, so every caller in this package (and every future one) applies
// goobernetes-smoke.md §5 rule 2 identically instead of hand-rolling the
// invalid-blocks-pass-or-fail rule per call site. observed=false with an
// empty precondition is a genuine FAIL — "nothing broke" must never be
// presentable when nothing ran (§5 rule 2 / D4), and the inverse holds too: a
// real negative result must never be swallowed into "invalid" just because
// something else about the run was imperfect.
func ClassifyItem(precondition PreconditionFailure, observed bool) (Verdict, string) {
	if precondition != "" {
		return VerdictInvalid, string(precondition)
	}
	if observed {
		return VerdictPass, ""
	}
	return VerdictFail, ""
}

// OverallVerdict combines per-item verdicts into the bundle's overall
// outcome (goobernetes-smoke.md §4: "All must pass in one procedure...
// cherry-picking passes across rebuilt clusters is a fail"). Any INVALID
// item makes the whole bundle invalid — an unproven item can never be
// papered over by other items passing. Absent that, any FAIL makes the
// bundle fail. Only when every item is a genuine pass does the bundle pass.
// An empty slice is INVALID: a bundle that asserts nothing proves nothing,
// the same "must never be a masked error" discipline ClassifyItem applies
// per-item, applied to the whole procedure.
func OverallVerdict(items []Verdict) Verdict {
	if len(items) == 0 {
		return VerdictInvalid
	}
	sawFail := false
	for _, v := range items {
		switch v {
		case VerdictInvalid:
			return VerdictInvalid
		case VerdictFail:
			sawFail = true
		}
	}
	if sawFail {
		return VerdictFail
	}
	return VerdictPass
}
