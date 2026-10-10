package mcpio

import (
	"errors"

	"github.com/goobers/goobers/internal/handoffcheck"
)

// PublicationReceiptFileName is the invocation-scoped JSONL log of
// schema-bound publish_output outcomes (#6868).
const PublicationReceiptFileName = "publication-receipts.jsonl"

// Publication receipt outcomes.
const (
	// PublicationAccepted: every schema-bound slot validated and the manifest
	// was written.
	PublicationAccepted = "accepted"
	// PublicationRejected: publish_output refused the manifest; nothing was
	// written.
	PublicationRejected = "rejected"
)

// PublicationReceipt is tool-owned diagnostic evidence of one schema-bound
// publish_output call. Like PublicationRejection it carries issue codes and
// JSON Pointer locations, never payload values.
type PublicationReceipt struct {
	Outcome        string                     `json:"outcome"`
	ManifestDigest string                     `json:"manifestDigest,omitempty"`
	Outputs        []PublicationReceiptOutput `json:"outputs,omitempty"`
}

// PublicationReceiptOutput is one slot's outcome within a PublicationReceipt.
// PayloadDigest names the exact staged bytes that passed validation; it is
// empty for a rejected slot.
type PublicationReceiptOutput struct {
	Slot          string               `json:"slot"`
	SchemaID      string               `json:"schemaId,omitempty"`
	Category      string               `json:"category,omitempty"`
	Issues        []handoffcheck.Issue `json:"issues,omitempty"`
	PayloadDigest string               `json:"payloadDigest,omitempty"`
}

// The receipt log is diagnostic: the harness re-validates the publication at
// the completion boundary, so a receipt that cannot be written never changes
// what publish_output reports to the agent.
func (t *Toolset) recordAcceptedPublication(manifestDigest string, outputs []PublicationReceiptOutput) {
	if len(t.schemas) == 0 {
		return
	}
	_ = t.appendReceipt(t.cfg.PublicationReceiptFile, "publication", PublicationReceipt{
		Outcome:        PublicationAccepted,
		ManifestDigest: manifestDigest,
		Outputs:        outputs,
	})
}

func (t *Toolset) recordRejectedPublication(err error) {
	var rejection *PublicationRejection
	if !errors.As(err, &rejection) {
		return
	}
	receipt := PublicationReceipt{Outcome: PublicationRejected}
	for _, out := range rejection.Outputs {
		receipt.Outputs = append(receipt.Outputs, PublicationReceiptOutput{
			Slot: out.Slot, SchemaID: out.SchemaID, Category: out.Category, Issues: out.Issues,
		})
	}
	_ = t.appendReceipt(t.cfg.PublicationReceiptFile, "publication", receipt)
}

// ReadPublicationReceipts reads the publication receipt log. A missing file
// means no schema-bound publish_output call was made.
func ReadPublicationReceipts(workspace, receiptFile string) ([]PublicationReceipt, error) {
	return readReceipts[PublicationReceipt](workspace, receiptFile, "publication")
}

// ResetPublicationReceipts removes any prior invocation's publication log.
func ResetPublicationReceipts(workspace, receiptFile string) error {
	return resetReceipts(workspace, receiptFile, "publication")
}
