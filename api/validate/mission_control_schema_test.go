package validate

import (
	"testing"

	"github.com/goobers/goobers/api/schemas"
)

func TestMissionControlSchemaRejectsWrongVersionAndThresholdShape(t *testing.T) {
	for name, document := range map[string]string{
		"wrong version":         `{"schemaVersion":"v2","generatedAt":"2026-08-07T07:00:00Z","evidence":[],"metrics":[],"subsystems":[],"overall":{"policy":{"unknown":"block"},"requiredSubsystemIds":[],"advisorySubsystemIds":[],"verdict":"unknown","reasonCode":"insufficient-evidence"}}`,
		"range without maximum": `{"schemaVersion":"goobers.dev/mission-control-verdict/v1alpha1","generatedAt":"2026-08-07T07:00:00Z","evidence":[],"metrics":[{"id":"latency","displayName":"Latency","subsystemId":"api","requirement":"required","criterion":{"comparator":"range-inclusive","unit":"ms","minimum":0},"displayPrecision":1,"observationWindow":{"start":"2026-08-07T06:55:00Z","end":"2026-08-07T07:00:00Z"},"requiredFreshness":"5m0s","verdict":"unknown","reasonCode":"missing"}],"subsystems":[],"overall":{"policy":{"unknown":"block"},"requiredSubsystemIds":[],"advisorySubsystemIds":[],"verdict":"unknown","reasonCode":"insufficient-evidence"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := newV(t).ValidateJSON(schemas.MissionControlVerdict, []byte(document)); err == nil {
				t.Fatal("schema validation succeeded")
			}
		})
	}
}
