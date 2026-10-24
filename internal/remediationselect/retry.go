package remediationselect

import "time"

// OperatorRetryRecord is the durable audit of the last operator-driven retry,
// embedded in the sticky remediation-state comment and the audit comment.
type OperatorRetryRecord struct {
	Route      string    `json:"route"`
	Actor      string    `json:"actor"`
	Reason     string    `json:"reason"`
	RecordedAt time.Time `json:"recordedAt"`
	HeadSHA    string    `json:"headSha"`
	BaseSHA    string    `json:"baseSha"`
}
