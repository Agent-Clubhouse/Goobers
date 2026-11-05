package fanout

import (
	"fmt"

	"github.com/goobers/goobers/internal/journal"
)

// ValidateSourceBlobRef checks the bounded content identity that a freeze
// activity can verify before reading any source bytes.
func ValidateSourceBlobRef(source journal.FanOutSourceRef, maxBytes int64) error {
	switch {
	case source.Visit == 0 || source.Attempt < 1:
		return &SourceError{Reason: ReasonProducer, Detail: "source must identify a settled producer visit and attempt"}
	case !ValidDigest(source.Digest):
		return &SourceError{Reason: ReasonMalformedDigest, Detail: fmt.Sprintf("source digest %q is malformed", source.Digest)}
	case source.MediaType != SourceMediaType:
		return &SourceError{
			Reason: ReasonMediaType,
			Detail: fmt.Sprintf("media type %q is not application/json", source.MediaType),
		}
	case maxBytes <= 0 || maxBytes > MaxSourceBytes:
		return fmt.Errorf("source byte limit %d is outside 1..%d", maxBytes, MaxSourceBytes)
	case source.Size < 0 || source.Size > maxBytes:
		return &SourceError{
			Reason: ReasonTooLarge,
			Detail: fmt.Sprintf("%d bytes exceeds the %d byte bound", source.Size, maxBytes),
		}
	}
	return nil
}
