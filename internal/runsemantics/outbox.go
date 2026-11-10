package runsemantics

import apiv1 "github.com/goobers/goobers/api/v1alpha1"

// OutboxExportFailureCode is the stable terminal code for missing declared
// outbox evidence.
const OutboxExportFailureCode = "outbox_export_failed"

// IsOutboxExportFailure reports a failed declared-evidence export. It is
// terminal even when the task otherwise allows command failures to continue.
func IsOutboxExportFailure(result apiv1.ResultEnvelope) bool {
	return result.Error != nil && result.Error.Code == OutboxExportFailureCode
}
