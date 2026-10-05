package sessioning

import (
	"errors"
	"regexp"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/workbench"
)

// MaxOperationWriteBytes bounds a single field command independently of reads.
const MaxOperationWriteBytes = 256 << 10

// BacklogEditRequest is a model-authored command, never an authority envelope.
// RequestID is scoped to the actual session turn by the host before acceptance.
type BacklogEditRequest struct {
	SourceBindingID string `json:"sourceBindingId"`
	RequestID       string `json:"requestId"`
	workbench.BacklogPatchRequest
}

// BacklogCapabilitiesRequest asks for current native field capabilities.
type BacklogCapabilitiesRequest struct {
	SourceBindingID string `json:"sourceBindingId"`
}

// BacklogReceiptRequest retrieves retained evidence under the original human.
type BacklogReceiptRequest struct {
	SourceBindingID string `json:"sourceBindingId"`
	CommandID       string `json:"commandId"`
}

var nativeLocatorPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
var nativeCommandPattern = regexp.MustCompile(`^workbench-[0-9a-f]{32}$`)

// DecodeBacklogEdit rejects undeclared authority and bounds one field edit.
func DecodeBacklogEdit(raw []byte) (BacklogEditRequest, error) {
	var request BacklogEditRequest
	allowed := map[string]bool{"sourceBindingId": true, "requestId": true, "id": true, "sourceId": true, "expectedRevision": true, "field": true, "value": true, "values": true}
	if err := decodeBoundedOperation(raw, allowed, &request, MaxOperationWriteBytes); err != nil {
		return request, err
	}
	return request, ValidateBacklogEdit(request)
}

// DecodeBacklogCapabilities decodes a closed source capability lookup.
func DecodeBacklogCapabilities(raw []byte) (BacklogCapabilitiesRequest, error) {
	var request BacklogCapabilitiesRequest
	if err := decodeOperation(raw, map[string]bool{"sourceBindingId": true}, &request); err != nil {
		return request, err
	}
	return request, ValidateBacklogCapabilities(request)
}

// DecodeBacklogReceipt decodes a closed retained-command lookup.
func DecodeBacklogReceipt(raw []byte) (BacklogReceiptRequest, error) {
	var request BacklogReceiptRequest
	if err := decodeOperation(raw, map[string]bool{"sourceBindingId": true, "commandId": true}, &request); err != nil {
		return request, err
	}
	return request, ValidateBacklogReceipt(request)
}

// ValidateBacklogCapabilities checks the configured source name.
func ValidateBacklogCapabilities(request BacklogCapabilitiesRequest) error {
	if !sourceBindingPattern.MatchString(request.SourceBindingID) {
		return errors.New("invalid backlog source binding")
	}
	return nil
}

// ValidateBacklogReceipt requires a canonical command locator.
func ValidateBacklogReceipt(request BacklogReceiptRequest) error {
	if !sourceBindingPattern.MatchString(request.SourceBindingID) || !nativeCommandPattern.MatchString(request.CommandID) {
		return errors.New("invalid backlog receipt locator")
	}
	return nil
}

// ValidateBacklogEdit checks structural native field bounds before authority or effects.
func ValidateBacklogEdit(request BacklogEditRequest) error {
	if !sourceBindingPattern.MatchString(request.SourceBindingID) || !operationText(request.RequestID, 128) || !nativeLocatorPattern.MatchString(request.ID) || !nativeLocatorPattern.MatchString(request.SourceID) || !operationText(request.ExpectedRevision, 256) {
		return errors.New("invalid backlog command identity")
	}
	switch request.Field {
	case "title", "description", "state":
		if request.Value == nil || request.Values != nil || !utf8.ValidString(*request.Value) || len(*request.Value) > 192<<10 {
			return errors.New("invalid scalar field edit")
		}
	case "labels", "assignees":
		if request.Value != nil || request.Values == nil || len(request.Values) > 128 {
			return errors.New("invalid set field edit")
		}
		for _, value := range request.Values {
			if !operationText(value, 400) {
				return errors.New("invalid set member")
			}
		}
	default:
		return errors.New("unsupported native field")
	}
	return nil
}
