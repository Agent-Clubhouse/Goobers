package eventing

import (
	"strings"
	"testing"
	"time"
)

func TestPinnedRoutingAndStartDocumentsAreClosed(t *testing.T) {
	route := Route{Consumer: "consumer", Revision: "revision", Workflow: "workflow", WorkflowDigest: "workflow-pin", GooberDigest: "goober-pin", ConfigGeneration: "config-pin"}
	raw, err := (Plan{Revision: "routing", Routes: []Route{route}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(string(raw), `"configGeneration":"config-pin"`, `"configGeneration":""`, 1), strings.Replace(string(raw), `"revision":"routing"`, `"revision":"routing","revision":"other"`, 1), strings.Replace(string(raw), `"revision":"routing"`, `"revision":"routing","unknown":true`, 1), string(raw) + `{}`} {
		if _, err := ParsePlan([]byte(bad)); err == nil {
			t.Fatal("invalid retained plan accepted")
		}
	}
	route.Debounce = &Debounce{Window: time.Second, MaxWait: time.Minute, MaxEvents: 10, InputMode: "all"}
	if _, err = (Plan{Revision: "routing", Routes: []Route{route}}).Marshal(); err == nil {
		t.Fatal("empty debounce key became global bucket")
	}
	route.FailureReason = "debounce key missing"
	if _, err = (Plan{Revision: "routing", Routes: []Route{route}}).Marshal(); err != nil {
		t.Fatal("failed delivery could not retain its configured target", err)
	}
	start := StartEnvelope{Kind: StartKind, Gaggle: "own", GroupID: "group", Consumer: "consumer", Revision: "revision", Workflow: "workflow", WorkflowDigest: "workflow-pin", GooberDigest: "goober-pin", ConfigGeneration: "config-pin", InputMode: "latest", SelectedReceipt: "receipt", EventCount: 3}
	raw, err = start.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseStart(raw); err != nil || got != start {
		t.Fatal("start pins changed", err)
	}
	if _, err = ParseStart([]byte(strings.Replace(string(raw), `"gaggle":"own"`, `"gaggle":"own","actor":"admin"`, 1))); err == nil {
		t.Fatal("start accepted authority override")
	}
}
