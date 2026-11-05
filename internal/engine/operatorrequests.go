package engine

import (
	"strings"

	"go.temporal.io/sdk/workflow"
)

const operatorRequestLedgerChange = "operator-request-key-ledger-v1"

func bindOperatorRequestLedger(ctx workflow.Context, hitl *hitlSession, gates *gateDecisionSession) {
	if !hitl.liveTerminal || workflow.GetVersion(ctx, operatorRequestLedgerChange, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return
	}
	shareOperatorRequestLedger(hitl, gates)
}

// Both protocols claim keys in one workflow-owned ledger. A claim tags the
// existing payload fingerprint with its protocol; it does not allocate a second
// key map. Legacy histories keep their original independent ledgers.
func shareOperatorRequestLedger(hitl *hitlSession, gates *gateDecisionSession) {
	gates.fingerprints = hitl.fingerprints
	hitl.sharedRequestKeys, gates.sharedRequestKeys = true, true
}

// Check-and-claim happens in the handlers without a workflow yield, after their
// mutex recheck and before publishing a decision or scheduling journal delivery.
// Temporal workflow coroutines therefore cannot interleave another claim.
// Validators only read; handlers also compare fingerprints after acquiring the
// mutex, so a stale query or validator snapshot cannot cross protocol ownership.
func operatorRequestFingerprint(shared bool, protocol, fingerprint string) string {
	if !shared {
		return fingerprint
	}
	return protocol + "\x00" + fingerprint
}

func operatorKeyOwnedByOther(shared bool, ledger map[string]string, protocol, id string) bool {
	if !shared {
		return false
	}
	prior, seen := ledger[id]
	return seen && !strings.HasPrefix(prior, protocol+"\x00")
}
