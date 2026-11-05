package runsemantics

import (
	"encoding/json"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestSharedStateRetainsPrivateFieldJSONShape(t *testing.T) {
	// These fields were private before extraction. Exporting them for the local
	// runner must not silently add workflow or recovery wire payload fields.
	outputs := NewStageOutputs()
	outputs.Record("stage", map[string]any{"value": "private"}, apiv1.IntegrityDerived)
	for _, tc := range []struct {
		value any
		want  string
	}{
		{outputs, `{"stage":{}}`},
		{StageUsageBudget{Totals: NewStageUsageTotals()}, `{}`},
	} {
		got, err := json.Marshal(tc.value)
		if err != nil || string(got) != tc.want {
			t.Fatalf("JSON=%s err=%v, want %s", got, err, tc.want)
		}
	}
}
