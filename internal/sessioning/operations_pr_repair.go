package sessioning

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/goobers/goobers/providers"
)

// MaxOperationPRRepairBytes caps the encoded one-commit intent independently of
// provider file/count bounds. The host still validates exact selected custody.
const MaxOperationPRRepairBytes = 2 << 20

// PRRepairInspectRequest advances only through this turn's confirmed receipt.
type PRRepairInspectRequest struct {
	ParentCommandID string `json:"parentCommandId,omitempty"`
}

// PRRepairReadRequest names a literal path at the selected or confirmed head.
type PRRepairReadRequest struct {
	Path            string `json:"path"`
	ParentCommandID string `json:"parentCommandId,omitempty"`
}

// PRRepairReceiptRequest refers only to this turn's selected PR command.
type PRRepairReceiptRequest struct {
	CommandID string `json:"commandId"`
}

var repairCommandPattern = regexp.MustCompile(`^repair-[0-9a-f]{32}$`)

// ValidatePRRepairInspect rejects another command namespace.
func ValidatePRRepairInspect(request PRRepairInspectRequest) error {
	if request.ParentCommandID != "" && !repairCommandPattern.MatchString(request.ParentCommandID) {
		return errors.New("invalid PR repair parent command")
	}
	return nil
}

// DecodePRRepairInspect accepts no model-authored target or identity.
func DecodePRRepairInspect(raw []byte) (PRRepairInspectRequest, error) {
	var request PRRepairInspectRequest
	if err := decodeOperation(raw, map[string]bool{"parentCommandId": true}, &request); err != nil {
		return request, err
	}
	return request, ValidatePRRepairInspect(request)
}

// ValidatePRRepairRead shares the native literal-path rules before provider I/O.
func ValidatePRRepairRead(request PRRepairReadRequest) error {
	if err := ValidatePRRepairInspect(PRRepairInspectRequest{ParentCommandID: request.ParentCommandID}); err != nil {
		return err
	}
	text := "path validation"
	return validatePRRepairNative([]PRRepairChange{{Path: request.Path, Content: &text}}, strings.Repeat("a", 40), "Read selected PR file")
}

// DecodePRRepairRead closes the path/parent request object.
func DecodePRRepairRead(raw []byte) (PRRepairReadRequest, error) {
	var request PRRepairReadRequest
	if err := decodeOperation(raw, map[string]bool{"path": true, "parentCommandId": true}, &request); err != nil {
		return request, err
	}
	return request, ValidatePRRepairRead(request)
}

// ValidatePRRepairRequest checks only bounded syntax, never permission or head
// authority. Native publication repeats the same checks against retained intent.
func ValidatePRRepairRequest(request PRRepairRequest) error {
	if !operationText(request.RequestID, 128) || !operationText(request.Rationale, 4096) || strings.TrimSpace(request.Rationale) == "" {
		return errors.New("invalid PR repair command")
	}
	if err := ValidatePRRepairInspect(PRRepairInspectRequest{ParentCommandID: request.ParentCommandID}); err != nil {
		return err
	}
	return validatePRRepairNative(request.Changes, request.ExpectedHeadSHA, request.Rationale)
}
func validatePRRepairNative(changes []PRRepairChange, head, rationale string) error {
	id := "repair-" + strings.Repeat("0", 32)
	target := providers.RepairPullRequest{Repository: providers.RepositoryRef{Provider: "github", Owner: "validation", Name: "validation"}, RepositoryID: "1", ID: "1", StableID: "1", Head: "repair", Base: "base", HeadSHA: head, BaseSHA: strings.Repeat("f", 40), Open: true}
	value := providers.PullRequestRepair{Target: target, CommandID: id, Message: "PR repair\n\n" + rationale + "\nGoobers-Repair: " + id + "\n"}
	for _, change := range changes {
		value.Changes = append(value.Changes, providers.PRRepairChange{Path: change.Path, PreviousBlob: change.PreviousBlob, Content: change.Content})
	}
	return providers.ValidatePullRequestRepair(value)
}

// DecodePRRepairRequest also rejects duplicate/case-folded/unknown nested file
// fields. Omitted content denotes deletion; explicit null is rejected.
func DecodePRRepairRequest(raw []byte) (PRRepairRequest, error) {
	var request PRRepairRequest
	allowed := map[string]bool{"requestId": true, "expectedHeadSha": true, "parentCommandId": true, "rationale": true, "changes": true}
	if err := decodeBoundedOperation(raw, allowed, &request, MaxOperationPRRepairBytes); err != nil {
		return request, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return request, err
	}
	var changes []json.RawMessage
	if err := json.Unmarshal(fields["changes"], &changes); err != nil {
		return request, err
	}
	for _, rawChange := range changes {
		var change PRRepairChange
		if err := decodeBoundedOperation(rawChange, map[string]bool{"path": true, "previousBlob": true, "content": true}, &change, MaxOperationPRRepairBytes); err != nil {
			return request, err
		}
	}
	return request, ValidatePRRepairRequest(request)
}

// ValidatePRRepairReceipt checks the dedicated command namespace.
func ValidatePRRepairReceipt(request PRRepairReceiptRequest) error {
	if !repairCommandPattern.MatchString(request.CommandID) {
		return errors.New("invalid PR repair receipt")
	}
	return nil
}

// DecodePRRepairReceipt rejects target/actor/credential injection.
func DecodePRRepairReceipt(raw []byte) (PRRepairReceiptRequest, error) {
	var request PRRepairReceiptRequest
	if err := decodeOperation(raw, map[string]bool{"commandId": true}, &request); err != nil {
		return request, err
	}
	return request, ValidatePRRepairReceipt(request)
}
