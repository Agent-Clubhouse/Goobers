package engine

import (
	"encoding/json"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// WithChildResult constructs a bounded return after the trusted finalizer has
// retained custody. It checks the accepted execution pins and omits bulk stage
// outputs. Validation alone does not verify blobs or authorize parent adoption.
// This helper does not enable generated-child execution.
func (r RunResult) WithChildResult(child triggerqueue.ChildRecord, result childresult.TerminalReturn) (RunResult, error) {
	if err := result.Validate(child); err != nil {
		return RunResult{}, err
	}
	result.Input.References = append([]string(nil), result.Input.References...)
	r.ChildResult = &result
	r.Outputs = nil
	encoded, err := json.Marshal(r)
	if err != nil || len(encoded) > childresult.MaxTerminalReturnBytes {
		return RunResult{}, childresult.ErrInvalidTerminalReturn
	}
	return r, nil
}
