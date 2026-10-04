package sessioning

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/workbench"
)

// MaxOperationResolutionBytes bounds one semantic assessment and evidence list.
const MaxOperationResolutionBytes = 32 << 10

// NeedsHumanResolutionRequest contains no host identity or credential fields.
type NeedsHumanResolutionRequest struct {
	SourceBindingID string `json:"sourceBindingId"`
	RequestID       string `json:"requestId"`
	workbench.NeedsHumanResolutionRequest
}

var resolutionCommandPattern = regexp.MustCompile(`^resolution-[0-9a-f]{32}$`)
var evidenceDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// DecodeNeedsHumanInspect requires the current stable source object identity.
func DecodeNeedsHumanInspect(raw []byte) (BacklogReadRequest, error) {
	request, err := DecodeBacklogRead(raw)
	if err == nil {
		err = ValidateNeedsHumanInspect(request)
	}
	return request, err
}

// ValidateNeedsHumanInspect prevents ambiguous locator-only inspections.
func ValidateNeedsHumanInspect(request BacklogReadRequest) error {
	if ValidateBacklogRead(request) != nil || !nativeLocatorPattern.MatchString(request.ExpectedSourceID) {
		return errors.New("invalid needs-human inspection identity")
	}
	return nil
}

// DecodeNeedsHumanResolution also closes nested evidence objects; duplicate or
// case-folded authority-shaped keys are rejected before typed decoding.
func DecodeNeedsHumanResolution(raw []byte) (NeedsHumanResolutionRequest, error) {
	var request NeedsHumanResolutionRequest
	allowed := map[string]bool{"sourceBindingId": true, "requestId": true, "id": true, "sourceId": true, "expectedRevision": true, "observationDigest": true, "basis": true, "rationale": true, "evidence": true}
	if err := decodeBoundedOperation(raw, allowed, &request, MaxOperationResolutionBytes); err != nil {
		return request, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return request, err
	}
	if err := closedResolutionEvidence(fields["basis"]); err != nil {
		return request, err
	}
	var evidence []json.RawMessage
	if err := json.Unmarshal(fields["evidence"], &evidence); err != nil {
		return request, err
	}
	for _, ref := range evidence {
		if err := closedResolutionEvidence(ref); err != nil {
			return request, err
		}
	}
	return request, ValidateNeedsHumanResolution(request)
}
func closedResolutionEvidence(raw []byte) error {
	var ref workbench.NeedsHumanEvidenceRef
	return decodeOperation(raw, map[string]bool{"kind": true, "id": true, "digest": true}, &ref)
}

// ValidateNeedsHumanResolution bounds the agent's assessment without pretending
// a syntactic check proves semantic completion or current source authority.
func ValidateNeedsHumanResolution(request NeedsHumanResolutionRequest) error {
	invalid := errors.New("invalid needs-human assessment or evidence")
	if !sourceBindingPattern.MatchString(request.SourceBindingID) || !operationText(request.RequestID, 128) || !nativeLocatorPattern.MatchString(request.ID) || !nativeLocatorPattern.MatchString(request.SourceID) || !operationText(request.ExpectedRevision, 256) || !evidenceDigestPattern.MatchString(request.ObservationDigest) {
		return invalid
	}
	if !utf8.ValidString(request.Rationale) || len(request.Rationale) > 8192 || strings.TrimSpace(request.Rationale) == "" || strings.ContainsRune(request.Rationale, 0) || len(request.Evidence) < 1 || len(request.Evidence) > 8 {
		return invalid
	}
	if request.Basis.Kind != "current-human-message" && request.Basis.Kind != "learned-record" {
		return invalid
	}
	for _, ref := range append([]workbench.NeedsHumanEvidenceRef{request.Basis}, request.Evidence...) {
		if !validResolutionEvidence(ref) {
			return invalid
		}
	}
	return nil
}
func validResolutionEvidence(ref workbench.NeedsHumanEvidenceRef) bool {
	if !operationText(ref.ID, 128) || !evidenceDigestPattern.MatchString(ref.Digest) {
		return false
	}
	switch ref.Kind {
	case "current-human-message", "learned-record", "session-message", "native-command", "source-comment":
		return true
	}
	return false
}

// DecodeNeedsHumanReceipt reads only a dedicated marker command.
func DecodeNeedsHumanReceipt(raw []byte) (BacklogReceiptRequest, error) {
	var request BacklogReceiptRequest
	if err := decodeOperation(raw, map[string]bool{"sourceBindingId": true, "commandId": true}, &request); err != nil {
		return request, err
	}
	return request, ValidateNeedsHumanReceipt(request)
}

// ValidateNeedsHumanReceipt keeps native field and marker command IDs separate.
func ValidateNeedsHumanReceipt(request BacklogReceiptRequest) error {
	if !sourceBindingPattern.MatchString(request.SourceBindingID) || !resolutionCommandPattern.MatchString(request.CommandID) {
		return errors.New("invalid needs-human receipt")
	}
	return nil
}
