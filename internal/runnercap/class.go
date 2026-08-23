package runnercap

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// The Kubernetes label vocabulary shared by every component that stamps,
// selects, or renders against a stage pod (delivery decisions 015/016,
// goobernetes-dispatcher.md §3, goobernetes-restrictions.md §6/§7). These are
// EXPORTED CONSTANTS on purpose: the dispatcher stamps them on the pods it
// creates, and the per-runner-class NetworkPolicy reference-manifest renderer
// selects on them — two producers of one contract. A string literal in either
// place is how the two drift, and a drifted runner-class label fails CLOSED
// into the decision-010 materialize hang (the pod matches only default-deny),
// which looks nothing like a label mismatch. This package is the single home
// because it is the stdlib-only leaf both internal/instance (the runners:
// inventory) and internal/workflow/v_3_0 (runsOn) already share.
const (
	// LabelRunnerClass is the label key attributing a stage pod to its runner
	// class — the per-class NetworkPolicy egress model's selection key
	// (goobernetes-restrictions.md §6, delivery decision 004). The dispatcher
	// derives its value with RunnerClassValue and refuses to create a pod on
	// any workflow/gaggle/stage attempt to influence it (dispatcher §3).
	LabelRunnerClass = "goobers.dev/runner-class"
	// LabelRole is the label key naming a pod's role in the Goobers topology;
	// stage pods always carry LabelRole=RoleStage. The gaggle-namespace
	// baseline policies ("no inbound to stage pods, ever") select on it.
	LabelRole = "goobers.dev/role"
	// RoleStage is the LabelRole value every dispatcher-created stage pod
	// carries.
	RoleStage = "stage"
	// LabelGaggleNamespace is the label key marking a NAMESPACE as a gaggle
	// namespace. The blob-endpoint ingress grant (decision 010/012,
	// dispatcher §2a) selects gaggle namespaces by this marker combined — in
	// the SAME peer element — with the stage-pod podSelector, so its key and
	// value are contract, not decoration. The label is operator/GitOps-applied
	// (the product does not label namespaces); this constant exists so the
	// renderer and any preflight read the one spelling.
	// NOTE: internal/operator separately stamps the same key on worker
	// Deployments with the owning Gaggle CR's namespace as the value — a
	// different object kind and value semantic; the namespace MARKER value is
	// GaggleNamespaceMarker.
	LabelGaggleNamespace = "goobers.dev/gaggle-namespace"
	// GaggleNamespaceMarker is the LabelGaggleNamespace value on a gaggle
	// NAMESPACE object.
	GaggleNamespaceMarker = "true"
	// AnnotationRunnerClassRestrictions is the ANNOTATION key carrying the
	// human-readable preimage of a runner-class label value on every rendered
	// per-class NetworkPolicy (issue #3568): the sorted restriction set the
	// RunnerClassValue selector was derived from, spelled as the canonical
	// effect names joined by commas (RunnerClassPreimage). The selector value
	// can be an opaque hash ("rc-3f9a2b…"), so without this preimage an
	// operator diagnosing a hung or denied pod cannot tell WHICH restriction
	// set a class is without re-running the function. It is deliberately an
	// annotation, never a label: decision 015 makes the label the ONE derived
	// selector value, and an annotation cannot become a selector by accident.
	AnnotationRunnerClassRestrictions = "goobers.dev/runner-class-restrictions"
)

// runnerClassSlugs maps each closed-list restriction effect to the short slug
// RunnerClassValue composes the label value from. Growing the closed list
// (a product decision, goobernetes-restrictions.md §2) grows this table in the
// same change; TestRunnerClassValueCoversClosedList pins the two together.
var runnerClassSlugs = map[Restriction]string{
	RestrictionEnvDefaultDeny:   "envdeny",
	RestrictionFSReadonly:       "fsro",
	RestrictionNetworkAllowlist: "netallow",
	RestrictionNetworkNone:      "netnone",
	RestrictionTmpEphemeral:     "tmpeph",
}

// RunnerClassUnrestricted is the runner-class value of the empty restriction
// set — a runner that declares no isolation effect.
const RunnerClassUnrestricted = "unrestricted"

// RunnerClassValue derives the goobers.dev/runner-class label VALUE from a
// resolved restriction set (delivery decision 015). It is the ONLY producer of
// runner-class values in the product: the dispatcher stamps pods with it, and
// the per-runner-class NetworkPolicy reference-manifest renderer selects with
// it, so the two agree by construction rather than by coincidence. Any
// repository that cannot import this function (e.g. a separate infra repo)
// consumes the rendered manifests, never a hand-maintained copy (delivery
// decision 016).
//
// The scheme, exactly:
//
//  1. De-duplicate the input and sort it by the canonical effect spelling
//     (byte order), so every permutation of one set yields one value.
//  2. Map each effect to its fixed slug (env:default-deny→envdeny,
//     fs:readonly-except-workspace→fsro, network:allowlist→netallow,
//     network:none→netnone, tmp:ephemeral→tmpeph) and join the slugs with
//     "-". The empty set is RunnerClassUnrestricted.
//  3. If any effect is outside the closed list, or the joined form would
//     exceed the 63-character Kubernetes label-value bound, fall back to
//     "rc-" + the first 16 hex characters of the SHA-256 of the sorted
//     canonical spellings joined by newlines — still deterministic, always a
//     valid label value, never a silent truncation two distinct sets could
//     collide into.
//
// Every output matches the Kubernetes label-value grammar
// ([a-z0-9A-Z]([a-z0-9A-Z._-]*[a-z0-9A-Z])? with at most 63 characters);
// TestRunnerClassValueIsAlwaysAValidLabelValue asserts it across the closed
// list's power set.
func RunnerClassValue(restrictions []string) string {
	sorted := CanonicalRestrictions(restrictions)
	if len(sorted) == 0 {
		return RunnerClassUnrestricted
	}

	slugs := make([]string, 0, len(sorted))
	known := true
	for _, effect := range sorted {
		slug, ok := runnerClassSlugs[Restriction(effect)]
		if !ok {
			known = false
			break
		}
		slugs = append(slugs, slug)
	}
	if known {
		if joined := strings.Join(slugs, "-"); len(joined) <= 63 {
			return joined
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return "rc-" + hex.EncodeToString(sum[:])[:16]
}

// CanonicalRestrictions is the shared canonicalization step under
// RunnerClassValue AND RunnerClassPreimage: de-duplicate, then sort by the
// canonical effect spelling (byte order). Every producer that needs to agree
// with the runner-class label value — the dispatcher's stamp, the renderer's
// selector, and the renderer's preimage annotation — derives from this ONE
// function's output, so the annotation can never describe a different set
// than the one the selector value was hashed from.
func CanonicalRestrictions(restrictions []string) []string {
	sorted := make([]string, 0, len(restrictions))
	seen := make(map[string]struct{}, len(restrictions))
	for _, effect := range restrictions {
		if _, dup := seen[effect]; dup {
			continue
		}
		seen[effect] = struct{}{}
		sorted = append(sorted, effect)
	}
	sort.Strings(sorted)
	return sorted
}

// RunnerClassPreimage renders a restriction set as the human-readable
// AnnotationRunnerClassRestrictions value: the canonical (deduplicated,
// sorted) effect spellings joined by commas. The empty set renders as the
// empty string. It is a pure function of the SAME canonical slice
// RunnerClassValue derives the label value from, so
// RunnerClassValue(ParseRunnerClassPreimage(RunnerClassPreimage(s))) ==
// RunnerClassValue(s) for every s — the round-trip the renderer's tests pin.
func RunnerClassPreimage(restrictions []string) string {
	return strings.Join(CanonicalRestrictions(restrictions), ",")
}

// ParseRunnerClassPreimage decodes an AnnotationRunnerClassRestrictions value
// back into the restriction set it names. Empty segments are dropped, so the
// empty string decodes to the empty set (the RunnerClassUnrestricted class).
func ParseRunnerClassPreimage(preimage string) []string {
	var out []string
	for _, part := range strings.Split(preimage, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
